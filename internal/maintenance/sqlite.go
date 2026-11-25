package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

func sqliteFile(path, mode string) string {
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = url.Values{"mode": {mode}, "_busy_timeout": {"5000"}, "_synchronous": {"FULL"}}.Encode()
	return u.String()
}

func onlineCopy(ctx context.Context, dstPath string, src *sql.DB) error {
	f, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dst, err := sql.Open("sqlite3", sqliteFile(dstPath, "rw"))
	if err != nil {
		return err
	}
	copyErr := copyDatabase(ctx, dst, src)
	return errors.Join(copyErr, dst.Close())
}

func copyDatabase(ctx context.Context, dst, src *sql.DB) error {
	source, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer source.Close()
	dest, err := dst.Conn(ctx)
	if err != nil {
		return err
	}
	defer dest.Close()
	return source.Raw(func(source any) error {
		return dest.Raw(func(dest any) (err error) {
			backup, err := dest.(*sqlite3.SQLiteConn).Backup("main", source.(*sqlite3.SQLiteConn), "main")
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, backup.Finish()) }()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				done, err := backup.Step(256)
				if err != nil || done {
					return err
				}
				// SQLite reports both busy and incomplete as done=false.
				timer := time.NewTimer(time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		})
	})
}

func openBackupDB(path string) (*sql.DB, error) {
	return openReadOnlyDB(path, false)
}

func openStandaloneDB(path string) (*sql.DB, error) {
	return openReadOnlyDB(path, true)
}

func openReadOnlyDB(path string, immutable bool) (*sql.DB, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database file %s must be a regular file", filepath.Base(path))
	}
	dsn := sqliteFile(path, "ro")
	if immutable {
		dsn += "&immutable=1"
	}
	return sql.Open("sqlite3", dsn)
}
