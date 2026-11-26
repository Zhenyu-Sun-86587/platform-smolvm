package nodegateway

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	wc "sandbox-platform/internal/runtime/workerclient"
	"strconv"
	"strings"
	"time"
)

func scopedBackup(id string, scope nodeproto.Route) bool {
	return nodeproto.ValidID(id) && strings.HasSuffix(id, "-"+nodeproto.BackupSuffix(scope.RuntimeInstanceID, scope.BundleDigest))
}

type backupSource struct {
	file     *os.File
	size     int64
	manifest string
}

func backupRPC[T any](s *Server, mux *http.ServeMux, slots chan struct{}, path string, open func(context.Context, nodeproto.Route, Route, T) (backupSource, error)) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		if !takeSlot(slots) {
			writeFailure(w, nodeproto.CodeBusy)
			return
		}
		defer func() { <-slots }()
		var envelope nodeproto.Envelope[T]
		if r.URL.RawQuery != "" || decodeJSON(http.MaxBytesReader(w, r.Body, nodeproto.ControlRequestLimit), &envelope) != nil {
			writeFailure(w, nodeproto.CodeInvalid)
			return
		}
		route, err := s.checkScope(envelope.Protocol, envelope.Authority, envelope.Route, true)
		if err != nil {
			writeFailure(w, nodeproto.FailureCode(err))
			return
		}
		source, err := open(r.Context(), envelope.Route, route, envelope.Request)
		if err != nil {
			writeFailure(w, nodeproto.FailureCode(err))
			return
		}
		defer source.file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set(nodeproto.BackupSizeHeader, strconv.FormatInt(source.size, 10))
		if source.manifest != "" {
			w.Header().Set(nodeproto.BackupManifestHeader, source.manifest)
		}
		w.Header().Set("Trailer", nodeproto.BackupDigestTrailer)
		w.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(w)
		deadline := func() time.Time {
			limit := time.Now().Add(30 * time.Second)
			if expiry, ok := r.Context().Deadline(); ok && expiry.Before(limit) {
				return expiry
			}
			return limit
		}
		if err := controller.SetWriteDeadline(deadline()); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		digest := sha256.New()
		buf := make([]byte, 64<<10)
		var sent int64
		for r.Context().Err() == nil {
			n, readErr := source.file.Read(buf)
			if n > 0 {
				if int64(n) > source.size-sent {
					return
				}
				if err := controller.SetWriteDeadline(deadline()); err != nil {
					return
				}
				if _, err = io.MultiWriter(w, digest).Write(buf[:n]); err != nil {
					return
				}
				sent += int64(n)
			}
			if readErr != nil {
				if readErr == io.EOF && sent == source.size {
					w.Header().Set(nodeproto.BackupDigestTrailer, fmt.Sprintf("sha256:%x", digest.Sum(nil)))
				}
				return
			}
		}
	})
}

func (s *Server) registerBackups(mux *http.ServeMux) {
	slots := make(chan struct{}, 2)
	backupRPC(s, mux, slots, nodeproto.BackupManifestPath, func(ctx context.Context, scope nodeproto.Route, r Route, in nodeproto.BackupSelection) (backupSource, error) {
		if !scopedBackup(in.ID, scope) || r.LoadManifest == nil || r.Reader != in.Reader || len(in.Captures)+len(in.Generations) > 4096 {
			return backupSource{}, rt.ErrInvalid
		}
		for _, ids := range [][]string{in.Captures, in.Generations} {
			for _, id := range ids {
				if !nodeproto.ValidID(id) {
					return backupSource{}, rt.ErrInvalid
				}
			}
		}
		raw, err := r.LoadManifest()
		if err != nil {
			return backupSource{}, err
		}
		f, d, err := r.Worker.PrepareBackup(ctx, wc.BackupSelection{ID: in.ID, Reader: r.Reader, Captures: in.Captures, Generations: in.Generations, BundleManifest: raw})
		if err != nil {
			return backupSource{}, err
		}
		info, err := f.Stat()
		if err != nil || info.Size() > 16<<20 || !nodeproto.ValidDigest(d) {
			f.Close()
			return backupSource{}, rt.ErrInvalid
		}
		return backupSource{file: f, size: info.Size(), manifest: d}, nil
	})
	backupRPC(s, mux, slots, nodeproto.BackupMemberPath, func(ctx context.Context, scope nodeproto.Route, r Route, in nodeproto.BackupMember) (backupSource, error) {
		if !scopedBackup(in.ID, scope) || !nodeproto.ValidMember(in.Member) {
			return backupSource{}, rt.ErrInvalid
		}
		f, size, err := r.Worker.OpenBackupMember(ctx, in.ID, in.Member)
		return backupSource{file: f, size: size}, err
	})
	scopedRPC(s, mux, nodeproto.BackupReleasePath, func(ctx context.Context, scope nodeproto.Route, r Route, in nodeproto.BackupMember) (nodeproto.WorkerResult[nodeproto.Empty], error) {
		if !scopedBackup(in.ID, scope) || in.Member != "" {
			return workerResult(nodeproto.Empty{}, rt.ErrInvalid)
		}
		return workerResult(nodeproto.Empty{}, r.Worker.ReleaseBackup(ctx, in.ID))
	})
}
