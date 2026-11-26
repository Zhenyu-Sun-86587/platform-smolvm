package nodeclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"

	"sandbox-platform/internal/nodeproto"
	wc "sandbox-platform/internal/runtime/workerclient"
)

type backupReader struct {
	response  *http.Response
	hash      hash.Hash
	remaining int64
}

func (r *backupReader) Close() error { return r.response.Body.Close() }
func (r *backupReader) Read(p []byte) (int, error) {
	n, err := r.response.Body.Read(p)
	if int64(n) > r.remaining {
		return 0, errors.New("backup member exceeded its receipt length")
	}
	r.remaining -= int64(n)
	r.hash.Write(p[:n])
	if err == io.EOF && (r.remaining != 0 || r.response.Trailer.Get(nodeproto.BackupDigestTrailer) != fmt.Sprintf("sha256:%x", r.hash.Sum(nil))) {
		err = errors.New("incomplete backup member or digest mismatch")
	}
	return n, err
}
func backupStream[T any](ctx context.Context, a *Adapter, path string, in T) (io.ReadCloser, int64, string, error) {
	if err := a.client.authorizeRequest(ctx); err != nil {
		return nil, 0, "", err
	}
	raw, err := json.Marshal(nodeproto.Envelope[T]{Protocol: nodeproto.Version, Authority: a.client.authority, Route: a.scope, Request: in})
	if err != nil {
		return nil, 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.client.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, "", err
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	r, err := a.client.stream.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	size, err := strconv.ParseInt(r.Header.Get(nodeproto.BackupSizeHeader), 10, 64)
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "application/octet-stream" || err != nil || size < 0 {
		r.Body.Close()
		return nil, 0, "", &nodeproto.Failure{Code: nodeproto.CodeUnavailable}
	}
	return &backupReader{response: r, hash: sha256.New(), remaining: size}, size, r.Header.Get(nodeproto.BackupManifestHeader), nil
}

func (a *Adapter) BackupManifest(ctx context.Context, in wc.BackupSelection) (io.ReadCloser, string, error) {
	r, size, d, err := backupStream(ctx, a, nodeproto.BackupManifestPath, nodeproto.BackupSelection{ID: in.ID, Reader: in.Reader, Captures: in.Captures, Generations: in.Generations})
	if err == nil && (!nodeproto.ValidDigest(d) || size > 16<<20) {
		r.Close()
		return nil, "", errors.New("invalid pinned manifest receipt")
	}
	return r, d, err
}
func (a *Adapter) BackupMember(ctx context.Context, id, name string) (io.ReadCloser, int64, error) {
	r, n, _, err := backupStream(ctx, a, nodeproto.BackupMemberPath, nodeproto.BackupMember{ID: id, Member: name})
	return r, n, err
}
func (a *Adapter) ReleaseBackup(ctx context.Context, id string) error {
	r, err := call[nodeproto.BackupMember, nodeproto.WorkerResult[nodeproto.Empty]](ctx, a, nodeproto.BackupReleasePath, nodeproto.BackupMember{ID: id})
	if err == nil && r.Fault != nil {
		return r.Fault
	}
	return err
}
