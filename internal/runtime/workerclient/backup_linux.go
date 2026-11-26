package workerclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type BackupCapture struct {
	CaptureID      string `json:"capture_id"`
	OperationID    string `json:"source_operation_id"`
	ManifestDigest string `json:"manifest_digest"`
}

type BackupSelection struct {
	ID             string          `json:"backup_id"`
	Reader         string          `json:"state_reader_id"`
	Captures       []BackupCapture `json:"-"`
	Generations    []string        `json:"generation_ids"`
	BundleManifest json.RawMessage `json:"bundle_manifest"`
}

func (c Client) PrepareBackup(ctx context.Context, selected BackupSelection) (*os.File, string, error) {
	ids := make([]string, 0, len(selected.Captures))
	for _, capture := range selected.Captures {
		ids = append(ids, capture.CaptureID)
	}
	if selected.Generations == nil {
		selected.Generations = []string{}
	}
	payload := struct {
		BackupSelection
		CaptureIDs []string `json:"capture_ids"`
	}{BackupSelection: selected, CaptureIDs: ids}
	result, file, err := c.call(ctx, request{Method: "PrepareBackupClosure", Payload: payload}, nil,
		&descriptor{ResourceID: selected.ID, Role: "backup_manifest"})
	if err != nil {
		return nil, "", err
	}
	var receipt struct {
		ID     string `json:"backup_id"`
		Digest string `json:"manifest_digest"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil || receipt.ID != selected.ID || !readOnlyFile(file) {
		_ = file.Close()
		return nil, "", errors.New("Worker backup manifest descriptor or scope differs")
	}
	return file, receipt.Digest, nil
}

func (c Client) OpenBackupMember(ctx context.Context, id, name string) (*os.File, int64, error) {
	result, file, err := c.call(ctx, request{Method: "ReadBackupMember", Payload: map[string]string{"backup_id": id, "member": name}}, nil,
		&descriptor{ResourceID: id, Role: "backup_member"})
	if err != nil {
		return nil, 0, err
	}
	var receipt struct {
		ID   string `json:"backup_id"`
		Name string `json:"member"`
		Size int64  `json:"bytes"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil || receipt.ID != id || receipt.Name != name || receipt.Size < 0 || !readOnlyFile(file) {
		_ = file.Close()
		return nil, 0, errors.New("Worker backup member descriptor or scope differs")
	}
	return file, receipt.Size, nil
}

func (c Client) ReleaseBackup(ctx context.Context, id string) error {
	_, _, err := c.call(ctx, request{Method: "ReleaseBackupClosure", Payload: map[string]string{"backup_id": id}}, nil, nil)
	return err
}

func readOnlyFile(file *os.File) bool {
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	info, statErr := file.Stat()
	return err == nil && flags&unix.O_ACCMODE == unix.O_RDONLY && flags&unix.O_PATH == 0 && statErr == nil && info.Mode().IsRegular()
}

type BackupControl interface {
	BackupManifest(context.Context, BackupSelection) (io.ReadCloser, string, error)
	BackupMember(context.Context, string, string) (io.ReadCloser, int64, error)
	ReleaseBackup(context.Context, string) error
}

func (c Client) BackupManifest(ctx context.Context, selection BackupSelection) (io.ReadCloser, string, error) {
	return c.PrepareBackup(ctx, selection)
}
func (c Client) BackupMember(ctx context.Context, id, name string) (io.ReadCloser, int64, error) {
	return c.OpenBackupMember(ctx, id, name)
}
