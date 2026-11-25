package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const DefaultLogRetention = 7 * 24 * time.Hour

type retentionRecord struct {
	Purged bool `json:"purged"`
}

type retainedRun struct {
	id       string
	bytes    int64
	finished time.Time
}

func (j journal) retainedOutput(task Task) (Task, error) {
	var record retentionRecord
	err := readJSON(j.run(task.RunID, "output-retention.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		return task, nil
	}
	if err != nil {
		return task, err
	}
	// Readers stop seeing segments before reclamation starts. The original
	// committed end and dropped counts still describe the preceding output.
	task.Stdout.RetainedFrom = task.Stdout.CommittedTo
	task.Stderr.RetainedFrom = task.Stderr.CommittedTo
	return task, nil
}

func (j journal) reclaimOutput(boot string, retention time.Duration, requested int64) (int64, error) {
	candidates, allocated, err := j.outputInventory(boot)
	if err != nil {
		return 0, err
	}
	free, err := diskAvailable(j.root)
	if err != nil {
		return 0, err
	}
	sort.Slice(candidates, func(i, k int) bool { return candidates[i].finished.Before(candidates[k].finished) })
	expiry := time.Now().Add(-retention)
	for _, item := range candidates {
		if item.finished.After(expiry) && allocated+requested <= guestOutputLimit &&
			(requested == 0 || free >= minimumFreeSpace+requested) {
			break
		}
		if err := j.purgeOutput(item.id); err != nil {
			return 0, err
		}
		allocated -= item.bytes
		free += item.bytes
	}
	return allocated, nil
}

func (j journal) outputInventory(boot string) ([]retainedRun, int64, error) {
	entries, err := os.ReadDir(j.run(""))
	if err != nil {
		return nil, 0, err
	}
	var candidates []retainedRun
	var allocated int64
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !identifier.MatchString(id) {
			continue
		}
		var request taskRequest
		if err := readJSON(j.run(id, "request.json"), &request); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, 0, err
		}
		var terminal result
		if err := readJSON(j.run(id, "result.json"), &terminal); errors.Is(err, os.ErrNotExist) {
			if request.BootID == boot {
				allocated += request.Spec.OutputLimit
				continue
			}
			// A known cold boot closes the old producer. Its partial logs
			// remain readable for the same retention window.
			terminal.Finished = request.CreatedAt
			for stream, log := range map[string]*LogIndex{"stdout": &terminal.Stdout, "stderr": &terminal.Stderr} {
				if err := readJSON(j.run(id, stream, "index.json"), log); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, 0, err
				}
			}
		} else if err != nil {
			return nil, 0, err
		}
		var record retentionRecord
		err := readJSON(j.run(id, "output-retention.json"), &record)
		if err == nil && record.Purged {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, 0, err
		}
		bytes := terminal.Stdout.CommittedTo + terminal.Stderr.CommittedTo
		allocated += bytes
		if bytes == 0 {
			continue
		}
		finished := terminal.Finished
		if err == nil {
			// An interrupted purge must finish even if the budget recovered.
			finished = time.Time{}
		}
		candidates = append(candidates, retainedRun{id: id, bytes: bytes, finished: finished})
	}
	return candidates, allocated, nil
}

func (j journal) purgeOutput(id string) error {
	marker := j.run(id, "output-retention.json")
	if err := writeJSON(marker, retentionRecord{}); err != nil {
		return err
	}
	for _, stream := range []string{"stdout", "stderr"} {
		dir := j.run(id, stream)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			number, ok := strings.CutSuffix(entry.Name(), ".bin")
			segment, err := strconv.ParseInt(number, 10, 64)
			if !ok || err != nil || segment < 0 || segmentName(segment) != entry.Name() || !entry.Type().IsRegular() {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return writeJSON(marker, retentionRecord{Purged: true})
}
