package tasks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type logBudget struct {
	mu        sync.Mutex
	remaining int64
}

func (budget *logBudget) take(bytes int64) int64 {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	bytes = min(bytes, budget.remaining)
	budget.remaining -= bytes
	return bytes
}

func diskAvailable(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * stat.Bsize, nil
}

func (j journal) outputAllocated() (int64, error) {
	entries, err := os.ReadDir(j.run(""))
	if err != nil {
		return 0, err
	}
	var allocated int64
	for _, e := range entries {
		if !e.IsDir() || !identifier.MatchString(e.Name()) {
			continue
		}
		var request taskRequest
		if err := readJSON(j.run(e.Name(), "request.json"), &request); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return 0, err
		}
		var terminal result
		if err := readJSON(j.run(e.Name(), "result.json"), &terminal); err == nil {
			allocated += terminal.Stdout.CommittedTo + terminal.Stderr.CommittedTo
		} else if errors.Is(err, os.ErrNotExist) {
			allocated += request.Spec.OutputLimit
		} else {
			return 0, err
		}
	}
	return allocated, nil
}

// collect drains the pipe even after a log error so the workload cannot
// deadlock while the runner waits for its exit.
func collect(j journal, runID, stream string, reader io.Reader, budget *logBudget, onDrop func()) (LogIndex, error) {
	buf := make([]byte, 64<<10)
	log := LogIndex{}
	var logErr error
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			writable := int64(0)
			if logErr == nil {
				writable = budget.take(int64(n))
				logErr = appendOutput(j, runID, stream, &log, buf[:writable])
			}
			if int64(n) > writable {
				log.Dropped += int64(n) - writable
				onDrop()
			}
			if logErr == nil {
				logErr = writeJSON(j.run(runID, stream, "index.json"), log)
			}
		}
		if readErr != nil {
			if readErr != io.EOF && logErr == nil {
				logErr = readErr
			}
			return log, logErr
		}
	}
}

func appendOutput(j journal, runID, stream string, log *LogIndex, data []byte) error {
	for len(data) > 0 {
		segment, offset := log.CommittedTo/segmentSize, log.CommittedTo%segmentSize
		length := min(int64(len(data)), segmentSize-offset)
		path := j.run(runID, stream, segmentName(segment))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteAt(data[:length], offset)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		file.Close()
		if writeErr != nil {
			return writeErr
		}
		log.CommittedTo += length
		data = data[length:]
	}
	return nil
}

func (j journal) readOutput(t Task, stream string, offset int64, limit int) (Output, error) {
	index := t.Stdout
	if stream == "stderr" {
		index = t.Stderr
	}
	start := min(offset, index.CommittedTo)
	end := start + min(int64(limit), index.CommittedTo-start)
	data := make([]byte, end-start)
	for pos := start; pos < end; {
		segment, skip := pos/segmentSize, pos%segmentSize
		length := min(end-pos, segmentSize-skip)
		file, err := os.Open(j.run(t.RunID, stream, segmentName(segment)))
		if err != nil {
			return Output{}, err
		}
		_, err = file.ReadAt(data[pos-start:pos-start+length], skip)
		file.Close()
		if err != nil {
			return Output{}, fmt.Errorf("read committed segment: %w", err)
		}
		pos += length
	}
	return Output{
		Stream: stream, Data: data, Start: start, End: end, Next: end,
		CommittedTo: index.CommittedTo, Dropped: index.Dropped,
		Gap:      offset > index.CommittedTo || (end == index.CommittedTo && index.Dropped > 0),
		Complete: t.Terminal() && end == index.CommittedTo,
	}, nil
}
