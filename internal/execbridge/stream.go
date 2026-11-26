package execbridge

import (
	"context"
	"errors"
	"sync"
	"time"

	rt "sandbox-platform/internal/runtime"
)

const retainedBytes = 512 << 10

type Snapshot struct {
	State        string           `json:"state"`
	Error        string           `json:"error,omitempty"`
	ExitCode     *int             `json:"exit_code,omitempty"`
	Output       []rt.OutputChunk `json:"output"`
	Next         uint64           `json:"next"`
	RetainedFrom uint64           `json:"retained_from"`
	End          uint64           `json:"end"`
	Gap          *rt.OutputGap    `json:"gap,omitempty"`
	StdinApplied uint64           `json:"stdin_seq"`
	Done         bool             `json:"done"`
}

type Reader func(context.Context, rt.ExecRead) (rt.ExecSession, error)

type Stream struct {
	mu      sync.Mutex
	changed chan struct{}
	state   Snapshot
	chunks  []rt.OutputChunk
}

func New(offset uint64, state string) *Stream {
	return &Stream{changed: make(chan struct{}), state: Snapshot{State: state, RetainedFrom: offset, End: offset}}
}

func (s *Stream) Observe(session rt.ExecSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Done {
		return
	}
	state := session.State
	done := session.Done && session.Next >= session.End
	if done && state != "failed" {
		state = "exited"
	}
	s.state.State, s.state.Done, s.state.ExitCode = state, done, session.ExitCode
	s.state.Error = ""
	if session.Error != nil {
		s.state.Error = *session.Error
	}
	s.state.StdinApplied = max(s.state.StdinApplied, session.StdinApplied)
	for _, chunk := range session.Output {
		if chunk.Offset > s.state.End {
			s.chunks = nil
			s.state.RetainedFrom, s.state.End = chunk.Offset, chunk.Offset
		}
		if end := chunk.Offset + uint64(len(chunk.Data)); end > s.state.End {
			chunk.Data = chunk.Data[s.state.End-chunk.Offset:]
			chunk.Offset = s.state.End
			s.chunks = append(s.chunks, chunk)
			s.state.End = end
		}
	}
	if session.Lost != nil && session.Lost.To > s.state.End {
		s.chunks = nil
		s.state.RetainedFrom, s.state.End = session.Lost.To, session.Lost.To
	}
	if s.state.End-s.state.RetainedFrom > retainedBytes {
		s.state.RetainedFrom = s.state.End - retainedBytes
		for len(s.chunks) > 0 && s.chunks[0].Offset+uint64(len(s.chunks[0].Data)) <= s.state.RetainedFrom {
			s.chunks[0] = rt.OutputChunk{}
			s.chunks = s.chunks[1:]
		}
	}
	s.notify()
}

func (s *Stream) Applied(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.StdinApplied = max(seq, s.state.StdinApplied)
}

func (s *Stream) Lost(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Done {
		return
	}
	s.state.State, s.state.Error, s.state.Done = "session_lost", message, true
	s.notify()
}

func (s *Stream) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Stream) Read(cursor uint64, limit int) (Snapshot, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := s.state
	view.Output = []rt.OutputChunk{}
	view.Next = cursor
	if cursor < view.RetainedFrom {
		view.Gap = &rt.OutputGap{From: cursor, To: view.RetainedFrom}
		view.Next = view.RetainedFrom
	}
	for _, chunk := range s.chunks {
		end := chunk.Offset + uint64(len(chunk.Data))
		if end <= view.Next || limit == 0 {
			continue
		}
		offset := max(chunk.Offset, view.Next)
		n := min(uint64(limit), end-offset)
		data := chunk.Data[offset-chunk.Offset : offset-chunk.Offset+n]
		view.Output = append(view.Output, rt.OutputChunk{Stream: chunk.Stream, Offset: offset, Data: data})
		view.Next = offset + n
		limit -= int(n)
	}
	return view, s.changed
}

func (s *Stream) Run(ctx context.Context, read Reader, observed func(Snapshot), release func()) {
	defer release()
	failures := 0
	for {
		current, _ := s.Read(0, 0)
		if current.Done {
			observed(current)
			return
		}
		session, err := read(ctx, rt.ExecRead{After: current.End, Wait: 1, Max: 64 << 10})
		if err == nil {
			s.Observe(session)
			failures = 0
			continue
		}
		failures++
		if errors.Is(err, rt.ErrNotFound) || failures >= 3 || ctx.Err() != nil {
			s.Lost("native exec session is unavailable")
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}
