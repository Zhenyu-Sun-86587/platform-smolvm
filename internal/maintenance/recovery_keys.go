package maintenance

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sandbox-platform/internal/config"
)

const recoveryFile = "recovery.sealed"
const recoveryVersion = "SandboxBackupSecrets/1"
const recoveryLimit = 64 << 20

var ErrRecoveryKey = errors.New("backup recovery key is missing or does not authenticate the archive")

func recoveryKeyPath(cfg config.Config, archive string, metadata Backup, explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, policy := range cfg.Backups {
		if policy.Scope == metadata.Scope && filepath.Join(policy.Directory, metadata.RunID) == filepath.Clean(archive) {
			return policy.RecoveryKeyFile
		}
	}
	return ""
}

func backupKey(path string) ([]byte, error) {
	if path == "" {
		return nil, ErrRecoveryKey
	}
	file, err := openFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return nil, ErrRecoveryKey
	}
	defer file.Close()
	mode, err := file.Stat()
	if err != nil || mode.Mode().Perm()&0077 != 0 || mode.Size() != 32 {
		return nil, ErrRecoveryKey
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(file, key); err != nil {
		return nil, ErrRecoveryKey
	}
	return key, nil
}

func backupCipher(path string) (cipher.AEAD, error) {
	key, err := backupKey(path)
	if err != nil {
		return nil, err
	}
	derived := hmac.New(sha256.New, key)
	_, _ = derived.Write([]byte(recoveryVersion))
	block, err := aes.NewCipher(derived.Sum(nil))
	clear(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func backupMAC(manifest Backup, path string) (string, error) {
	key, err := backupKey(path)
	if err != nil {
		return "", err
	}
	hash := hmac.New(sha256.New, key)
	clear(key)
	manifest.MAC = ""
	_, _ = hash.Write([]byte("SandboxBackupManifest/2\x00"))
	if err := json.NewEncoder(hash).Encode(manifest); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func recoveryPaths(cfg config.Config) map[string]string {
	secrets := filepath.Join(cfg.StateDir, "secrets")
	return map[string]string{
		"signer":       cfg.Storage.KeyFile,
		"task-env-key": filepath.Join(secrets, "task-env-key-v1"),
		"task-env":     filepath.Join(secrets, "task-env"),
		"task-env-v1":  filepath.Join(secrets, "task-env-v1"),
		"ssh":          filepath.Join(secrets, "ssh"),
	}
}

func recoveryDestination(paths map[string]string, member string) string {
	dir, name, hasName := strings.Cut(member, "/")
	switch dir {
	case "signer", "task-env-key":
		if !hasName {
			return paths[dir]
		}
	case "task-env", "task-env-v1", "ssh":
		if hasName && name != "." && !strings.HasPrefix(name, ".") && filepath.IsLocal(name) && filepath.Base(name) == name {
			return filepath.Join(paths[dir], name)
		}
	}
	return ""
}

func exportRecovery(ctx context.Context, db *sql.DB, cfg config.Config, output string, plan BackupPlan) ([]File, error) {
	aead, err := backupCipher(plan.RecoveryKeyFile)
	if err != nil {
		return nil, err
	}
	recovery := map[string][]byte{}
	remaining := int64(recoveryLimit)
	read := func(path, key string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := openFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, remaining+1))
		if err != nil {
			return err
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return errors.New("encrypted backup recovery material exceeds the supported size")
		}
		recovery[key] = data
		return nil
	}
	for name, path := range recoveryPaths(cfg) {
		if name == "signer" || name == "task-env-key" {
			if err := read(path, name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		entries, err := os.ReadDir(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if err := read(filepath.Join(path, entry.Name()), name+"/"+entry.Name()); err != nil {
				return nil, err
			}
		}
	}
	if err := verifyTaskRecovery(ctx, db, recovery); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(recovery)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(append([]byte{}, nonce...), nonce, raw, []byte(plan.ID))
	if err := os.WriteFile(filepath.Join(output, recoveryFile), sealed, 0600); err != nil {
		return nil, err
	}
	entry, err := digestFile(output, recoveryFile, "")
	return []File{entry}, err
}

func verifyRecovery(archive, id, keyFile string, verified map[string]File) (map[string][]byte, error) {
	if _, exists := verified[recoveryFile]; !exists {
		return nil, errors.New("backup has no encrypted recovery material")
	}
	recovery, err := openRecovery(archive, id, keyFile)
	if err != nil {
		return nil, err
	}
	delete(verified, recoveryFile)
	return recovery, nil
}

func openRecovery(archive, id, keyFile string) (map[string][]byte, error) {
	aead, err := backupCipher(keyFile)
	if err != nil {
		return nil, err
	}
	file, err := openFile(archive, recoveryFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	sealed, err := io.ReadAll(io.LimitReader(file, recoveryLimit*2+1))
	if err != nil || len(sealed) > recoveryLimit*2 || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("invalid encrypted recovery material")
	}
	raw, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(id))
	if err != nil {
		return nil, ErrRecoveryKey
	}
	var recovery map[string][]byte
	if err := json.Unmarshal(raw, &recovery); err != nil {
		return nil, err
	}
	return recovery, nil
}

func verifyTaskRecovery(ctx context.Context, db *sql.DB, recovery map[string][]byte) error {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT json_extract(request_json,'$.env_digest') FROM operations
		WHERE kind='task_start' AND COALESCE(json_extract(request_json,'$.env_digest'),'')<>''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	key := recovery["task-env-key"]
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return err
		}
		raw := recovery["task-env/"+digest]
		if sealed := recovery["task-env-v1/"+digest]; len(sealed) != 0 {
			block, err := aes.NewCipher(key)
			if err != nil {
				return errors.New("backup task environment key is unavailable")
			}
			aead, err := cipher.NewGCM(block)
			if err != nil || len(sealed) < 4+aead.NonceSize()+aead.Overhead() || !bytes.HasPrefix(sealed, []byte("SAE1")) {
				return errors.New("invalid encrypted task environment in backup")
			}
			raw, err = aead.Open(nil, sealed[4:4+aead.NonceSize()], sealed[4+aead.NonceSize():], []byte(digest))
			if err != nil {
				return errors.New("backup task environment integrity check failed")
			}
		}
		hash := sha256.Sum256(raw)
		if len(raw) == 0 || hex.EncodeToString(hash[:]) != digest {
			return errors.New("backup is missing a referenced task environment")
		}
	}
	return rows.Err()
}

func restoreRecovery(cfg config.Config, recovery map[string][]byte, uid, gid int) error {
	paths := recoveryPaths(cfg)
	missing := make(map[string][]byte)
	for member, data := range recovery {
		path := recoveryDestination(paths, member)
		if path == "" {
			return errors.New("backup recovery member is outside of the installation key store")
		}
		file, err := openFile(filepath.Dir(path), filepath.Base(path))
		if errors.Is(err, os.ErrNotExist) {
			missing[path] = data
			continue
		}
		if err != nil {
			return err
		}
		actual, err := io.ReadAll(io.LimitReader(file, int64(len(data))+1))
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		if !hmac.Equal(data, actual) {
			return errors.New("existing recovery material differs from the authenticated backup")
		}
	}
	for path, data := range missing {
		if err := installRecoveryFile(path, data, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func installRecoveryFile(path string, data []byte, uid, gid int) error {
	dir := filepath.Dir(path)
	if err := ensureRecoveryDirectory(dir, uid, gid); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".restore-key-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chown(uid, gid); err != nil {
		_ = file.Close()
		return err
	}
	_, err = file.Write(data)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil {
		return err
	}
	return syncPath(dir)
}

func ensureRecoveryDirectory(path string, uid, gid int) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("recovery key parent must be a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureRecoveryDirectory(filepath.Dir(path), uid, gid); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}
