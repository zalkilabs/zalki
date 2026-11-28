package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// A client is configured only when it is on this machine: its configuration
// directory exists, or the command that starts it is found. Files with no client
// are always configured. The rule is the same for every client and is checked on
// every run, so a client that appears after an install is configured by the next run.

// clientGroup is the declared files of one client.
type clientGroup struct {
	id, label, executable string
	files                 []ConfigFile
}

// clientGroups groups the declared files by client, ordered by client name.
// Files with no client are not in a group.
func clientGroups(declarations map[string]ConfigFile) []clientGroup {
	byID := map[string]*clientGroup{}
	for _, file := range declarations {
		if file.Client == "" {
			continue
		}
		group := byID[file.Client]
		if group == nil {
			group = &clientGroup{id: file.Client, label: file.ClientLabel, executable: file.Executable}
			byID[file.Client] = group
		}
		group.files = append(group.files, file)
	}
	groups := make([]clientGroup, 0, len(byID))
	for _, group := range byID {
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].id < groups[j].id })
	return groups
}

// present reports whether the client is on this machine.
func (group clientGroup) present(request Request) bool {
	for _, file := range group.files {
		target, err := file.target(request.HomeDir)
		if err != nil {
			continue
		}
		// A file directly in the home directory says nothing: that directory always exists.
		if dir := filepath.Dir(target); dir != request.HomeDir && isDirectory(dir) {
			return true
		}
	}
	return group.executable != "" && request.findExecutable(group.executable)
}

func (request Request) findExecutable(name string) bool {
	if request.FindExecutable != nil {
		return request.FindExecutable(name)
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// labels joins the display names of the groups for one line of output.
func labels(groups []clientGroup) string {
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, labelOf(group))
	}
	return strings.Join(names, ", ")
}

func labelOf(group clientGroup) string {
	if group.label != "" {
		return group.label
	}
	return group.id
}

// notFoundNote is the one line for the clients that are not on this machine.
func notFoundNote(absent []clientGroup) string {
	return fmt.Sprintf("Not found: %s. Install one, then run zalki install.", labels(absent))
}

// noClientError says that nothing would be configured.
func noClientError(action string, all []clientGroup) error {
	return fmt.Errorf("No supported client found: %s. Install one, then run zalki %s.", labels(all), action) //nolint:revive // error-strings: the exact CLI diagnostic contract includes a final period.
}

// skippedNotes are the two lines for a client whose file cannot be used.
func skippedNotes(group clientGroup, reason, action string) []string {
	return []string{
		fmt.Sprintf("Skipped %s: %s.", labelOf(group), reason),
		fmt.Sprintf("Next: fix that file, then run zalki %s.", action),
	}
}
