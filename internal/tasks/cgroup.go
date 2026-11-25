package tasks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const defaultCgroupRoot = "/sys/fs/cgroup"
const payloadPrefix = "sandbox-payload-"

func bootID() (string, error) {
	value, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(value)), err
}

func processStart(pid int) (string, error) {
	value, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	closeParen := strings.LastIndex(string(value), ")")
	if closeParen < 0 {
		return "", errors.New("invalid process identity")
	}
	fields := strings.Fields(string(value[closeParen+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", errors.New("process exited or missing start time")
	}
	return fields[19], nil
}

func makeCgroup(root, id string) (string, error) {
	path := filepath.Join(root, payloadPrefix+id)
	if err := os.Mkdir(path, 0755); err != nil {
		return "", err
	}
	return path, nil
}

func cgroupIdentity(path string) (uint64, uint64, error) {
	var stat unix.Stat_t
	err := unix.Lstat(path, &stat)
	if err == nil && stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		err = errors.New("cgroup path is not a directory")
	}
	return stat.Ino, stat.Dev, err
}

func verifyCgroup(root, runID, boot string, identity Identity) error {
	if !identifier.MatchString(runID) || identity.Cgroup != filepath.Join(root, payloadPrefix+runID) || identity.BootID != boot {
		return errors.New("cgroup identity does not match current guest boot and run")
	}
	inode, device, err := cgroupIdentity(identity.Cgroup)
	if err != nil {
		return err
	}
	if inode != identity.Inode || device != identity.Device {
		return errors.New("cgroup identity changed")
	}
	return nil
}

func writeCgroup(path, file, value string) error {
	control, err := os.OpenFile(filepath.Join(path, file), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer control.Close()
	_, err = control.WriteString(value)
	return err
}

type cgroupState struct {
	populated bool
	frozen    bool
}

func readCgroup(path string) (cgroupState, error) {
	var state cgroupState
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return state, err
	}
	var populationKnown, frozenKnown bool
	for _, line := range strings.Split(string(data), "\n") {
		switch line {
		case "populated 0", "populated 1":
			populationKnown = true
			state.populated = line == "populated 1"
		case "frozen 0", "frozen 1":
			frozenKnown = true
			state.frozen = line == "frozen 1"
		}
	}
	if !populationKnown || !frozenKnown {
		return state, errors.New("cgroup.events lacks population or freezer state")
	}
	return state, nil
}

func waitCgroup(path string, condition func(cgroupState) bool) error {
	end := time.Now().Add(3 * time.Second)
	for {
		state, err := readCgroup(path)
		if err != nil {
			return err
		}
		if condition(state) {
			return nil
		}
		if time.Now().After(end) {
			return errors.New("cgroup condition remained unconfirmed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func killCgroup(path string) error {
	return writeCgroup(path, "cgroup.kill", "1")
}

// Opening a pidfd before the second membership check prevents signalling a
// reused PID when the original member exits during cancellation.
func termCgroup(path string) error {
	return filepath.WalkDir(path, func(dir string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		procs := filepath.Join(dir, "cgroup.procs")
		initial, err := os.ReadFile(procs)
		if err != nil {
			return err
		}
		for _, value := range strings.Fields(string(initial)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				continue
			}
			fd, err := unix.PidfdOpen(pid, 0)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return err
			}
			members, err := os.ReadFile(procs)
			if err == nil {
				for _, member := range strings.Fields(string(members)) {
					if member == value {
						err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
						break
					}
				}
			}
			unix.Close(fd)
			if err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
		}
		return nil
	})
}

func probeCgroup(root string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return err
	}
	if stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return errors.New("cgroup v2 is unavailable")
	}
	path, err := makeCgroup(root, fmt.Sprintf("probe-%d", os.Getpid()))
	if err != nil {
		return err
	}
	defer os.Remove(path)
	child := exec.Command("/bin/sleep", "15")
	if err := child.Start(); err != nil {
		return err
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if err := writeCgroup(path, "cgroup.procs", strconv.Itoa(child.Process.Pid)); err != nil {
		return err
	}
	if err := writeCgroup(path, "cgroup.freeze", "1"); err != nil {
		return err
	}
	if err := waitCgroup(path, func(state cgroupState) bool { return state.frozen && state.populated }); err != nil {
		return err
	}
	if err := killCgroup(path); err != nil {
		return err
	}
	return waitCgroup(path, func(state cgroupState) bool { return !state.populated })
}
