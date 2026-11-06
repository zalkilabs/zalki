package procwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Read returns a snapshot of pid, preserving OS inspection errors.
func Read(pid int) (Info, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	b, err := os.ReadFile(filepath.Join(dir, "stat")) //nolint:gosec // G304: the PID is a decimal integer below the fixed /proc root.
	if err != nil {
		return Info{}, err
	}
	// comm may contain spaces and parentheses. Numeric fields start after its
	// final closing parenthesis; starttime is field 22 in proc_pid_stat(5).
	text := string(b)
	left, right := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if left < 0 || right <= left {
		return Info{}, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(text[right+1:])
	if len(fields) < 20 {
		return Info{}, fmt.Errorf("incomplete process stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return Info{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return Info{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Info{}, err
	}
	return Info{PID: pid, PPID: parent, Start: strings.TrimSpace(string(boot)) + ":" + fields[19],
		Name: text[left+1 : right], UID: info.Sys().(*syscall.Stat_t).Uid, Zombie: fields[0] == "Z"}, nil
}

// Named returns non-zombie processes owned by this user with the exact name.
func Named(name string) ([]Info, error) {
	rows, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []Info
	for _, row := range rows {
		pid, err := strconv.Atoi(row.Name())
		if err != nil {
			continue
		}
		p, err := Read(pid)
		if err == nil && p.Name == name && p.UID == uint32(os.Getuid()) && !p.Zombie { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
			result = append(result, p)
		}
	}
	return result, nil
}
