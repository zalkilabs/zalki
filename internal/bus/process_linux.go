package bus

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func readProcess(pid int) (processInfo, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return processInfo{}, err
	}
	// comm may contain spaces and parentheses. Numeric fields start after its
	// final closing parenthesis; starttime is field 22 in proc_pid_stat(5).
	text := string(b)
	left, right := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if left < 0 || right <= left {
		return processInfo{}, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(text[right+1:])
	if len(fields) < 20 {
		return processInfo{}, fmt.Errorf("incomplete process stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return processInfo{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return processInfo{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return processInfo{}, err
	}
	return processInfo{PID: pid, PPID: parent, Start: strings.TrimSpace(string(boot)) + ":" + fields[19],
		Name: text[left+1 : right], UID: info.Sys().(*syscall.Stat_t).Uid, Zombie: fields[0] == "Z"}, nil
}

func namedProcesses(name string) ([]processInfo, error) {
	rows, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []processInfo
	for _, row := range rows {
		pid, err := strconv.Atoi(row.Name())
		if err != nil {
			continue
		}
		p, err := readProcess(pid)
		if err == nil && p.Name == name && p.UID == uint32(os.Getuid()) && !p.Zombie {
			result = append(result, p)
		}
	}
	return result, nil
}
