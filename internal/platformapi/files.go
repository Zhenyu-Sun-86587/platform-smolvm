package platformapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/guestfiles"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/tasks"
)

const uploadLimit = rt.PublicFileUploadLimit

func (s *Server) fileRoutes() {
	const files = "/v1/machines/{machine}/files"
	s.route("GET "+files, s.downloadFile)
	s.route("GET "+files+"/list", s.listFiles)
	s.route("PUT "+files, s.uploadFile)
}

func guestPath(r *http.Request, directory bool) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" && directory {
		p = "/workspace"
	}
	if !path.IsAbs(p) || path.Clean(p) != p || len(p) > 4096 || strings.ContainsRune(p, 0) || !directory && p == "/" {
		return "", registry.ErrInvalid
	}
	return p, nil
}

func nativeFileBinding(b registry.Binding) bool {
	return b.RuntimeBundleID != "" || b.RuntimeInstanceID != ""
}

func (s *Server) openNativeFiles(ctx context.Context, p auth.Principal, b registry.Binding, adapter rt.Adapter, capability string) (*guestfiles.Session, error) {
	authorize := func(ctx context.Context) error {
		if err := s.CheckAccessPrincipal(ctx, p, b.MachineID); err != nil {
			return err
		}
		m, err := s.DB.GetMachine(ctx, b.MachineID)
		if err != nil {
			return err
		}
		c := m.CurrentBinding
		if m.DeletedAt != nil || c == nil || c.ID != b.ID || c.Generation != b.Generation || c.ExecutionRef() != b.ExecutionRef() || c.PowerState != registry.PowerRunning {
			return registry.ErrGeneration
		}
		return nil
	}
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	session, err := guestfiles.Open(ctx, adapter, b, authorize)
	if errors.Is(err, rt.ErrUnsupported) {
		return nil, &rt.CapabilityError{Capability: capability, Status: rt.Capability{
			Status: "unsupported", ReasonCode: "GUEST_FILE_TRANSFER_UNSUPPORTED",
			Message: "This execution does not support native Guest file transfer.",
		}}
	}
	return session, err
}

type guestFileEntry = guestfiles.Entry

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	machine := r.PathValue("machine")
	if err := p.Authorize(auth.ActionManage, machine); err != nil {
		return err
	}
	target, err := guestPath(r, true)
	if err != nil {
		return err
	}
	b, release, err := s.Access(r.Context(), p, machine)
	if err != nil {
		return err
	}
	defer release()
	adapter, err := s.ForBinding(b)
	if err != nil {
		return err
	}
	if nativeFileBinding(b) {
		if r.URL.Query().Get("path") == "" {
			target = "/"
		}
		session, err := s.openNativeFiles(r.Context(), p, b, adapter, rt.FileListCapability)
		if err != nil {
			return err
		}
		defer session.Close()
		entries, err := session.List(target)
		if err != nil {
			return err
		}
		return list(w, r, entries, func(e guestFileEntry) string { return e.Name })
	}
	res, err := adapter.Exec(r.Context(), b.RuntimeName, rt.ExecRequest{
		Command: []string{tasks.TaskctlPath, "list-files", target}, TimeoutSeconds: 10,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &lifecycle.Failure{Code: "GUEST_LIST_FAILED", Message: "guest directory could not be listed"}
	}
	var entries []guestFileEntry
	if err := json.Unmarshal(res.Stdout, &entries); err != nil {
		return err
	}
	return list(w, r, entries, func(e guestFileEntry) string { return e.Name })
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	machine := r.PathValue("machine")
	if err := p.Authorize(auth.ActionManage, machine); err != nil {
		return err
	}
	target, err := guestPath(r, false)
	if err != nil {
		return err
	}
	b, release, err := s.Access(r.Context(), p, machine)
	if err != nil {
		return err
	}
	defer release()
	adapter, err := s.ForBinding(b)
	if err != nil {
		return err
	}
	var session *guestfiles.Session
	if nativeFileBinding(b) {
		session, err = s.openNativeFiles(r.Context(), p, b, adapter, rt.FileReadCapability)
		if err != nil {
			return err
		}
		defer session.Close()
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "sandbox")
	dst := &fileResponse{w: w}
	if session != nil {
		err = session.Download(target, dst)
	} else {
		err = adapter.DownloadFile(r.Context(), b.RuntimeName, target, dst)
	}
	if err != nil && !dst.started {
		w.Header().Del("Content-Disposition")
	}
	if err != nil && dst.started {
		panic(http.ErrAbortHandler)
	}
	return err
}

type fileResponse struct {
	w       http.ResponseWriter
	started bool
}

func (f *fileResponse) Write(data []byte) (int, error) {
	f.started = true
	return f.w.Write(data)
}

type uploadIntent struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type uploadResult struct {
	ID    string `json:"upload_id"`
	State string `json:"state"`
	uploadIntent
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	machine := r.PathValue("machine")
	if err := p.Authorize(auth.ActionManage, machine); err != nil {
		return err
	}
	target, err := guestPath(r, false)
	if err != nil {
		return err
	}
	if r.ContentLength > uploadLimit {
		return &lifecycle.Failure{Code: "FILE_TOO_LARGE", Message: "upload limit is 64 MiB"}
	}
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		return &lifecycle.Failure{Code: "RESOURCE_EXHAUSTED", Message: "too many concurrent file uploads"}
	}
	file, err := os.CreateTemp(s.Config.StateDir, ".upload-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(r.Body, uploadLimit+1))
	if err != nil {
		return err
	}
	if n > uploadLimit {
		return &lifecycle.Failure{Code: "FILE_TOO_LARGE", Message: "upload limit is 64 MiB"}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ctx := r.Context()
	intent := uploadIntent{Path: target, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	env := envelope(r, p, intent)
	if r.Header.Get("If-Match") != "" {
		env.ExpectedRevision, err = revision(r)
		if err != nil {
			return err
		}
	}
	id, err := registry.NewID("upload_")
	if err != nil {
		return err
	}
	reservation := uploadResult{ID: id, State: "outcome_unknown", uploadIntent: intent}
	var binding string
	raw, replay, err := s.DB.Mutate(ctx, env, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		binding, err = checkAccessRevision(ctx, tx, machine, env.ExpectedRevision, "")
		return registry.MutationResult{Response: lifecycle.JSON(reservation)}, err
	})
	if err != nil {
		return err
	}
	if replay {
		var prior uploadResult
		if err := json.Unmarshal(raw, &prior); err != nil {
			return err
		}
		switch prior.State {
		case "uploaded":
			return write(w, 200, prior)
		case "failed_before_commit":
			return &lifecycle.Failure{Code: "FILE_UPLOAD_NOT_COMMITTED", Message: "the original Guest upload ended before committing the file"}
		default:
			return uploadUnknown()
		}
	}

	complete := func(state string) (uploadResult, error) {
		result := reservation
		result.State = state
		receipt, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return result, s.DB.CompleteMutation(receipt, env, raw, lifecycle.JSON(result))
	}
	failed := func(err error) error {
		_, _ = complete("failed_before_commit")
		return err
	}
	b, release, err := s.Access(ctx, p, machine)
	if err != nil {
		return failed(err)
	}
	defer release()
	if b.ID != binding {
		return failed(registry.ErrGeneration)
	}
	adapter, err := s.ForBinding(b)
	if err != nil {
		return failed(err)
	}
	if nativeFileBinding(b) {
		session, err := s.openNativeFiles(ctx, p, b, adapter, rt.FileWriteCapability)
		if err != nil {
			return failed(err)
		}
		defer session.Close()
		if err = session.Upload(target, file, n); err != nil {
			if errors.Is(err, rt.ErrUncertain) {
				return uploadUnknown()
			}
			_ = session.Close()
			return failed(err)
		}
	} else if err := adapter.UploadFileWithOptions(ctx, b.RuntimeName, target, file, rt.FileOptions{Size: n}); err != nil {
		if errors.Is(err, rt.ErrUncertain) {
			return uploadUnknown()
		}
		return err
	}
	result, err := complete("uploaded")
	if err != nil {
		return uploadUnknown()
	}
	return write(w, 200, result)
}

func uploadUnknown() error {
	return &lifecycle.Failure{Code: "OUTCOME_UNKNOWN", Message: "guest upload outcome needs verification at the original path"}
}
