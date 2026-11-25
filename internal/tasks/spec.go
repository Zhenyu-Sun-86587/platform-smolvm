package tasks

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func normalize(spec Spec) (Spec, error) {
	if len(spec.Argv) == 0 || len(spec.Argv) > 1024 || spec.Argv[0] == "" {
		return spec, errors.New("argv requires a command and at most 1024 entries")
	}
	for _, arg := range spec.Argv {
		if strings.ContainsRune(arg, 0) {
			return spec, errors.New("argv contains a NUL byte")
		}
	}
	if spec.Cwd == "" {
		spec.Cwd = "/workspace"
	}
	if !filepath.IsAbs(spec.Cwd) || strings.ContainsRune(spec.Cwd, 0) {
		return spec, errors.New("cwd must be an absolute Guest path")
	}
	spec.Cwd = filepath.Clean(spec.Cwd)
	if spec.User == "" {
		spec.User = "root"
	}
	if !identifier.MatchString(spec.User) {
		return spec, errors.New("user must be a Guest username or numeric UID")
	}
	for key, value := range spec.Env {
		if !envName.MatchString(key) || strings.ContainsRune(value, 0) {
			return spec, errors.New("invalid environment variable")
		}
	}
	if spec.OutputPolicy == "" {
		spec.OutputPolicy = "truncate"
	}
	if spec.OutputPolicy != "truncate" && spec.OutputPolicy != "cancel" {
		return spec, errors.New("output_policy must be truncate or cancel")
	}
	if spec.OutputLimit == 0 {
		spec.OutputLimit = defaultOutputLimit
	}
	if spec.OutputLimit < 0 || spec.OutputLimit > guestOutputLimit {
		return spec, errors.New("output_limit is outside the supported range")
	}
	return spec, nil
}

func HashSpec(spec Spec) (string, error) {
	normalized, err := normalize(spec)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return hash(payload), nil
}

func guestUser(name string) (*user.User, error) {
	if _, err := strconv.ParseUint(name, 10, 32); err == nil {
		return user.LookupId(name)
	}
	return user.Lookup(name)
}

func userEnvironment(spec Spec, account *user.User) (map[string]string, []string) {
	env := map[string]string{
		"HOME": account.HomeDir, "USER": account.Username, "LOGNAME": account.Username,
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG": "C.UTF-8", "PWD": spec.Cwd,
	}
	for key, value := range spec.Env {
		env[key] = value
	}
	values := make([]string, 0, len(env))
	for key, value := range env {
		values = append(values, key+"="+value)
	}
	sort.Strings(values)
	return env, values
}

func runWithCredentials(account *user.User) error {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	if uid == 0 && gid == 0 {
		return nil
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return err
	}
	groups := make([]int, 0, len(groupIDs))
	for _, group := range groupIDs {
		groupID, err := strconv.Atoi(group)
		if err != nil {
			return err
		}
		groups = append(groups, groupID)
	}
	if err := syscall.Setgroups(groups); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	return syscall.Setuid(uid)
}

func execPayload(spec Spec) error {
	account, err := guestUser(spec.User)
	if err != nil {
		return err
	}
	env, values := userEnvironment(spec, account)
	if err := runWithCredentials(account); err != nil {
		return err
	}
	if err := os.Chdir(spec.Cwd); err != nil {
		return err
	}
	command := spec.Argv[0]
	if strings.ContainsRune(command, '/') {
		return syscall.Exec(command, spec.Argv, values)
	}
	var denied bool
	for _, dir := range filepath.SplitList(env["PATH"]) {
		err := syscall.Exec(filepath.Join(dir, command), spec.Argv, values)
		switch {
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR):
		case errors.Is(err, syscall.EACCES):
			denied = true
		default:
			return err
		}
	}
	if denied {
		return syscall.EACCES
	}
	return syscall.ENOENT
}
