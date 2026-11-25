package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	segmentSize        = 8 << 20
	defaultOutputLimit = 256 << 20
	guestOutputLimit   = 2 << 30
	minimumFreeSpace   = 64 << 20
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type taskRequest struct {
	ID         string    `json:"id"`
	RunID      string    `json:"run_id"`
	Binding    Binding   `json:"binding"`
	StartToken string    `json:"start_token"`
	SpecHash   string    `json:"spec_hash"`
	Spec       Spec      `json:"spec"`
	CreatedAt  time.Time `json:"created_at"`
	BootID     string    `json:"boot_id"`
}

type taskIndex struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id"`
	StartToken      string `json:"start_token,omitempty"`
	SpecHash        string `json:"spec_hash"`
	OriginTaskID    string `json:"origin_task_id,omitempty"`
	OriginMachineID string `json:"origin_machine_id,omitempty"`
}

type taskOwner struct {
	Binding     Binding `json:"binding"`
	TaskID      string  `json:"task_id"`
	OperationID string  `json:"operation_id,omitempty"`
}

type result struct {
	State    string    `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Signal   string    `json:"signal,omitempty"`
	Finished time.Time `json:"finished_at"`
	Stdout   LogIndex  `json:"stdout"`
	Stderr   LogIndex  `json:"stderr"`
}

type cancelIntent struct {
	OperationID string    `json:"operation_id"`
	KillAt      time.Time `json:"kill_at"`
	Reason      string    `json:"reason"`
}

type journal struct {
	root string
}

func (j journal) run(id string, parts ...string) string {
	return filepath.Join(append([]string{j.root, "runs", id}, parts...)...)
}

func (j journal) indices(namespace string) string {
	return filepath.Join(j.root, "namespaces", namespace, "tasks")
}

func (j journal) index(namespace, id string) string {
	return filepath.Join(j.indices(namespace), id+".json")
}

func (j journal) taskIndices(namespace string) ([]taskIndex, error) {
	entries, err := os.ReadDir(j.indices(namespace))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var indexes []taskIndex
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !identifier.MatchString(id) {
			continue
		}
		var index taskIndex
		if err := readJSON(j.index(namespace, id), &index); err != nil {
			return nil, err
		}
		if index.ID != id || !identifier.MatchString(index.RunID) {
			return nil, errors.New("invalid task index")
		}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return decode(file, value)
}

func mkdir(path string) error {
	if err := os.Mkdir(path, 0700); errors.Is(err, fs.ErrExist) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("journal path is not a directory")
		}
		return syncDir(filepath.Dir(path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		return syncDir(filepath.Dir(path))
	}
	if err := mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	return mkdir(path)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func writeJSON(path string, value any) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".record-")
	if err != nil {
		return err
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func removeRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func hash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func deriveID(prefix string, parts ...string) string {
	return prefix + hash([]byte(strings.Join(parts, "\x00")))[:32]
}

func segmentName(segment int64) string {
	return strconv.FormatInt(segment, 10) + ".bin"
}
