package guestfiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

func fakeAgent(t *testing.T, ctx context.Context, authorize func(context.Context) error, capability bool, serve func(net.Conn)) (*Session, error) {
	t.Helper()
	host, guest := net.Pipe()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer guest.Close()
		expectRequest(t, guest, "ping")
		pong := map[string]any{"status": "pong", "version": 1}
		if capability {
			pong["capabilities"] = []string{Capability}
		}
		replyFrame(t, guest, pong)
		if serve != nil {
			serve(guest)
		}
	}()
	t.Cleanup(func() {
		_ = host.Close()
		<-finished
	})
	return open(ctx, host, authorize)
}

func expectRequest(t *testing.T, conn net.Conn, method string) request {
	t.Helper()
	var length [4]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		t.Errorf("read request header: %v", err)
		return request{}
	}
	data := make([]byte, binary.BigEndian.Uint32(length[:]))
	var req request
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Errorf("read request: %v", err)
	} else if err := json.Unmarshal(data, &req); err != nil || req.Method != method {
		t.Errorf("got request %q, want %q (%v)", req.Method, method, err)
	}
	return req
}

func replyFrame(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Error(err)
		return
	}
	frame := make([]byte, 4, len(data)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	if _, err := conn.Write(append(frame, data...)); err != nil {
		t.Error(err)
	}
}

func allow(context.Context) error { return nil }

func TestSystemFileListAndBinaryDownload(t *testing.T) {
	contents := []byte{0x00, 0xff, 0x00, 0x04}
	s, err := fakeAgent(t, t.Context(), allow, true, func(conn net.Conn) {
		req := expectRequest(t, conn, "list_directory")
		if req.Path != "/workspace" || req.Target == nil || req.Target.Kind != "vm" {
			t.Errorf("invalid list target %#v", req)
		}
		replyFrame(t, conn, map[string]any{"status": "ok", "data": map[string]any{"entries": []any{
			map[string]any{"name": "dir", "kind": "dir", "size": 0},
			map[string]any{"name": "binary", "kind": "file", "size": len(contents)},
		}}})
		req = expectRequest(t, conn, "file_read")
		if req.Path != "/workspace/binary" || req.Target == nil || req.Target.Kind != "vm" {
			t.Errorf("invalid download target %#v", req)
		}
		replyFrame(t, conn, map[string]any{"status": "data_chunk", "data": contents, "done": false})
		replyFrame(t, conn, map[string]any{"status": "data_chunk", "data": "", "done": true})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := s.List("/workspace")
	if err != nil || len(entries) != 2 || entries[0].Type != "directory" {
		t.Fatalf("list = %#v, %v", entries, err)
	}
	var got bytes.Buffer
	if err := s.Download("/workspace/binary", &got); err != nil || !bytes.Equal(got.Bytes(), contents) {
		t.Fatalf("download = %x, %v", got.Bytes(), err)
	}
}

func TestSystemFileUploadReceipt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		receipt string
		want    error
	}{
		{name: "empty", receipt: "confirmed"},
		{name: "beyond_private_limit", size: 700 << 10, receipt: "confirmed"},
		{name: "lost", size: 1, want: rt.ErrUncertain},
		{name: "mismatched", size: 1, receipt: "mismatched", want: rt.ErrUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Repeat([]byte{0xfe}, tc.size)
			s, err := fakeAgent(t, t.Context(), allow, true, func(conn net.Conn) {
				req := expectRequest(t, conn, "file_write_begin")
				if req.TotalSize == nil || *req.TotalSize != int64(tc.size) || req.Target == nil || req.Target.Kind != "vm" {
					t.Errorf("invalid upload begin %#v", req)
				}
				replyFrame(t, conn, map[string]any{"status": "ok"})
				hash := sha256.New()
				n := 0
				for n < tc.size {
					req := expectRequest(t, conn, "file_write_chunk")
					var body []byte
					if err := json.Unmarshal(req.Data, &body); err != nil || req.Done == nil || *req.Done || len(body) == 0 || len(body) > chunkSize {
						t.Errorf("invalid upload data chunk (%d bytes): %v", len(body), err)
						return
					}
					hash.Write(body)
					n += len(body)
					replyFrame(t, conn, map[string]any{"status": "ok"})
				}
				want := sha256.Sum256(data)
				if n != tc.size || !bytes.Equal(hash.Sum(nil), want[:]) {
					t.Error("staging changed byte length or contents")
				}
				req = expectRequest(t, conn, "file_write_chunk")
				if req.Done == nil || !*req.Done || string(req.Data) != `""` {
					t.Errorf("invalid commit %#v", req)
				}
				switch tc.receipt {
				case "confirmed":
					replyFrame(t, conn, map[string]any{"status": "progress"})
					replyFrame(t, conn, map[string]any{"status": "ok", "data": map[string]int{"bytes_written": tc.size}})
				case "mismatched":
					replyFrame(t, conn, map[string]any{"status": "ok", "data": map[string]int{"bytes_written": tc.size + 1}})
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Upload("/workspace/data", bytes.NewReader(data), int64(tc.size)); !errors.Is(err, tc.want) {
				t.Fatalf("upload error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSystemFileUploadDoesNotCommitBeforeValidatedSourceAndAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		size       int64
		want       error
	}{
		{"truncated", "x", 2, rt.ErrInvalid},
		{"oversized", "xy", 1, rt.ErrInvalid},
		{"revoked", "x", 1, auth.ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, err := fakeAgent(t, t.Context(), func(context.Context) error {
				calls++
				if calls > 1 {
					return auth.ErrForbidden
				}
				return nil
			}, true, func(conn net.Conn) {
				expectRequest(t, conn, "file_write_begin")
				replyFrame(t, conn, map[string]any{"status": "ok"})
				if tc.name != "truncated" {
					req := expectRequest(t, conn, "file_write_chunk")
					if req.Done == nil || *req.Done {
						t.Error("data chunk attempted to commit")
						return
					}
					replyFrame(t, conn, map[string]any{"status": "ok"})
				}
				var trailing [1]byte
				if _, err := conn.Read(trailing[:]); !errors.Is(err, io.EOF) {
					t.Errorf("got another frame after precommit rejection: %v", err)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Upload("/workspace/data", bytes.NewBufferString(tc.data), tc.size); !errors.Is(err, tc.want) {
				t.Fatalf("upload error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSystemFileCapabilityAndWorkerFencing(t *testing.T) {
	if s, err := fakeAgent(t, t.Context(), allow, false, nil); s != nil || !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("legacy agent = %v, %v", s, err)
	}
	err := workerError(&workerclient.Fault{Code: "E_STALE_EXECUTION"})
	if !errors.Is(err, registry.ErrGeneration) {
		t.Fatalf("stale Worker execution = %v", err)
	}
}

func TestSystemFileCanceledDownloadDoesNotReturnSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := fakeAgent(t, ctx, allow, true, func(conn net.Conn) {
		expectRequest(t, conn, "file_read")
		replyFrame(t, conn, map[string]any{"status": "data_chunk", "data": "AQ==", "done": false})
		cancel()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got bytes.Buffer
	if err := s.Download("/workspace/data", &got); err == nil {
		t.Fatal("missing terminal response was accepted")
	}
}
