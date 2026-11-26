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
	"sync/atomic"
	"time"

	"sandbox-platform/internal/nodeproto"
	wc "sandbox-platform/internal/runtime/workerclient"
)

type backupReader struct {
	response  *http.Response
	hash      hash.Hash
	remaining int64
	ctx       context.Context
	cancel    context.CancelFunc
	terminal  error
}

func (r *backupReader) Close() error {
	r.cancel()
	return r.response.Body.Close()
}
func (r *backupReader) Read(p []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.response.Body.Read(p)
	if int64(n) > r.remaining {
		r.terminal = errors.New("backup member exceeded its receipt length")
		r.cancel()
		return 0, r.terminal
	}
	r.remaining -= int64(n)
	r.hash.Write(p[:n])
	if err == io.EOF && (r.remaining != 0 || r.response.Trailer.Get(nodeproto.BackupDigestTrailer) != fmt.Sprintf("sha256:%x", r.hash.Sum(nil))) {
		err = errors.New("incomplete backup member or digest mismatch")
	}
	if err != nil {
		r.terminal = err
		r.cancel()
	}
	return n, err
}
func backupStream[T any](ctx context.Context, a *Adapter, path string, in T) (_ io.ReadCloser, _ int64, _ string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	check, stop := context.WithTimeout(ctx, 2*time.Second)
	err = a.client.authorizeRequest(check)
	stop()
	if err != nil {
		return nil, 0, "", err
	}
	raw, err := json.Marshal(nodeproto.Envelope[T]{Protocol: nodeproto.Version, Authority: a.client.authority, Route: a.scope, Request: in})
	if err != nil {
		return nil, 0, "", err
	}
	var sending atomic.Bool
	req, err := http.NewRequestWithContext(tracePeer(ctx, cancel, &sending), http.MethodPost, a.client.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, "", err
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	r, err := a.client.stream.Do(req)
	if err != nil {
		return nil, 0, "", transportError(path == nodeproto.BackupManifestPath && sending.Load())
	}
	if r.TLS == nil || nodeproto.PeerDeadline(r.TLS.VerifiedChains).IsZero() {
		r.Body.Close()
		return nil, 0, "", &nodeproto.Failure{Code: nodeproto.CodeAuthority}
	}
	expiry := nodeproto.PeerDeadline(r.TLS.VerifiedChains)
	size, err := strconv.ParseInt(r.Header.Get(nodeproto.BackupSizeHeader), 10, 64)
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "application/octet-stream" || err != nil || size < 0 {
		r.Body.Close()
		return nil, 0, "", &nodeproto.Failure{Code: nodeproto.CodeUnavailable}
	}
	go a.client.watchAuthority(ctx.Done(), expiry, cancel)
	return &backupReader{response: r, hash: sha256.New(), remaining: size, ctx: ctx, cancel: cancel}, size, r.Header.Get(nodeproto.BackupManifestHeader), nil
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
