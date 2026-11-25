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

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/tasks"
)

const uploadLimit = 64 << 20

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
	res, err := s.Runtime.Exec(r.Context(), b.RuntimeName, rt.ExecRequest{
		Command: []string{tasks.TaskctlPath, "list-files", target}, TimeoutSeconds: 10,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &lifecycle.Failure{Code: "GUEST_LIST_FAILED", Message: "guest directory could not be listed"}
	}
	type entry struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	var entries []entry
	if err := json.Unmarshal(res.Stdout, &entries); err != nil {
		return err
	}
	return list(w, r, entries, func(e entry) string { return e.Name })
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
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "sandbox")
	dst := &fileResponse{w: w}
	err = s.Runtime.DownloadFile(r.Context(), b.RuntimeName, target, dst)
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
		if prior.State != "uploaded" {
			return uploadUnknown()
		}
		return write(w, 200, prior)
	}

	b, release, err := s.Access(ctx, p, machine)
	if err != nil {
		return err
	}
	defer release()
	if b.ID != binding {
		return registry.ErrGeneration
	}
	if err := s.Runtime.UploadFileWithOptions(ctx, b.RuntimeName, target, file, rt.FileOptions{Size: n}); err != nil {
		if errors.Is(err, rt.ErrUncertain) {
			return uploadUnknown()
		}
		return err
	}
	result := reservation
	result.State = "uploaded"
	if err := s.DB.CompleteMutation(ctx, env, raw, lifecycle.JSON(result)); err != nil {
		return uploadUnknown()
	}
	return write(w, 200, result)
}

func uploadUnknown() error {
	return &lifecycle.Failure{Code: "OUTCOME_UNKNOWN", Message: "guest upload outcome needs verification at the original path"}
}
