package nodegateway

import (
	"context"
	"crypto/sha256"
	"errors"
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
		limit := int64(nodeproto.ControlRequestLimit)
		if path == nodeproto.BackupManifestPath {
			limit = nodeproto.BackupSelectionLimit
		}
		if r.URL.RawQuery != "" || decodeJSON(http.MaxBytesReader(w, r.Body, limit), &envelope) != nil {
			writeFailure(w, nodeproto.CodeInvalid)
			return
		}
		route, err := s.checkScope(path, envelope.Protocol, envelope.Authority, envelope.Node, envelope.Route, true)
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
		if !scopedBackup(in.ID, scope) || r.LoadManifest == nil || r.Reader != in.Reader || len(in.Captures)+len(in.Generations) == 0 || len(in.Captures)+len(in.Generations) > 4096 {
			return backupSource{}, rt.ErrInvalid
		}
		proof, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := verifyBackupCaptures(proof, scope, in.Captures, r.Worker.QueryOperation); err != nil {
			return backupSource{}, err
		}
		for _, id := range in.Generations {
			if !nodeproto.ValidID(id) {
				return backupSource{}, rt.ErrInvalid
			}
			if err := r.Worker.VerifyBackupGeneration(proof, id); err != nil {
				return backupSource{}, backupOwnershipError(err)
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

func backupOwnershipError(err error) error {
	var fault *wc.Fault
	if errors.As(err, &fault) && (fault.Code == "E_NOT_FOUND" || fault.Code == "E_BUNDLE_UNAVAILABLE") {
		return rt.ErrConflict
	}
	return err
}

func verifyBackupCaptures(ctx context.Context, scope nodeproto.Route, captures []wc.BackupCapture,
	query func(context.Context, string) (rt.OperationReceipt, error)) error {
	seen := make(map[string]bool, len(captures))
	for _, capture := range captures {
		if !nodeproto.ValidID(capture.CaptureID) || !nodeproto.ValidID(capture.OperationID) ||
			!nodeproto.ValidDigest(capture.ManifestDigest) || seen[capture.CaptureID] {
			return rt.ErrInvalid
		}
		seen[capture.CaptureID] = true
	}
	for _, capture := range captures {
		receipt, err := query(ctx, capture.OperationID)
		if err != nil {
			return backupOwnershipError(err)
		}
		if !nodeproto.OwnsManagedInstance(scope.LocalInstanceID, receipt.InstanceID) ||
			receipt.RouteID != "deep" || receipt.BundleDigest != scope.BundleDigest {
			return &nodeproto.Failure{Code: nodeproto.CodeRoute}
		}
		point := receipt.StatePoint
		if receipt.OperationID != capture.OperationID || point == nil {
			return rt.ErrConflict
		}
		switch receipt.Kind {
		case rt.VMSnapshot, rt.Hibernate:
			if receipt.Phase != "COMPLETED" || point.CaptureID != capture.CaptureID || point.ManifestDigest != capture.ManifestDigest {
				return rt.ErrConflict
			}
		case rt.VMRollback:
			// The rollback receipt also names its target StatePoint. Only the
			// rescue point was created by this operation and belongs to it.
			if point.RescueCaptureID != capture.CaptureID || point.RescueManifestDigest != capture.ManifestDigest {
				return rt.ErrConflict
			}
		default:
			return rt.ErrConflict
		}
	}
	return nil
}
