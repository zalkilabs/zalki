package install

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Service states, from positive evidence only.
const (
	serviceLoaded      = "loaded"
	serviceNotLoaded   = "not_loaded"
	serviceUnknown     = "unknown"
	serviceDisabled    = "disabled"
	serviceUnsupported = "unsupported"
)

// Loaded-configuration match of a loaded job against the owned tuple.
const (
	loadedMatches  = "matches"
	loadedMismatch = "mismatch"
	loadedUnknown  = "unknown"
)

// launchctlPath is the absolute service manager; adapter tests point it at
// an owned fake executable, never at the real one.
var launchctlPath = "/bin/launchctl"

const (
	launchctlOutput  = 1 << 20 // a real `print gui/UID` is about 100 KiB
	launchctlObserve = 2 * time.Second
	loginItemOpLimit = 10 * time.Second
)

// launchctlRunner runs /bin/launchctl with literal arguments and returns
// its bounded stdout and exit status. Tests replace it; CI never reaches
// the real service manager.
type launchctlRunner func(ctx context.Context, args ...string) ([]byte, int, error)

var runLaunchctl launchctlRunner = realLaunchctl

// loginItemsSupported: login integration is macOS only. Tests set it.
var loginItemsSupported = runtime.GOOS == "darwin"

func realLaunchctl(ctx context.Context, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, launchctlPath, args...)
	cmd.Env = []string{"HOME=/var/empty", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	// exec copies stdout into the pipe in its own goroutine; after the
	// command ends (or the deadline kills it) WaitDelay closes its side even
	// if a descendant still holds the output, so the call is always bounded.
	cmd.WaitDelay = time.Second
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	type result struct {
		out  []byte
		over bool
	}
	read := make(chan result, 1)
	go func() {
		// More than the limit is an error: a huge output is unknown, never
		// half-parsed. The rest is drained so the copier never blocks.
		out, _ := io.ReadAll(io.LimitReader(reader, launchctlOutput+1))
		_, _ = io.Copy(io.Discard, reader)
		read <- result{out, len(out) > launchctlOutput}
	}()
	waitErr := cmd.Run()
	_ = writer.Close()
	got := <-read
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil || errors.As(waitErr, &exit) && exit.ExitCode() < 0:
		// A deadline, a cancellation or a signal ended the command: whether
		// the service manager finished is unknown, never an exit status.
		return nil, 0, errors.Join(errors.New("launchctl ended without an exit status"), ctx.Err())
	case errors.Is(waitErr, exec.ErrWaitDelay):
		return nil, 0, errors.New("launchctl output stayed open after it ended") // unknown as well
	case got.over:
		return nil, 0, errors.New("launchctl output exceeds its limit")
	case errors.As(waitErr, &exit):
		return got.out, exit.ExitCode(), nil
	}
	return got.out, 0, waitErr
}

// serviceObservation is one read-only look at the job.
type serviceObservation struct {
	state   string
	loaded  string // matches, mismatch or unknown
	program string
	args    []string
	// absent is positive evidence that no job has the label: a complete
	// service list of the domain without it. A disabled label does not
	// change it.
	absent bool
	// disabledKnown says the disabled list was read and recognised; every
	// start needs it (removal may rely on the job's identity alone).
	disabledKnown bool
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// observeService looks at the job without changing anything: the job
// itself, the domain's complete service list, and its disabled list. A
// failed or unrecognised command is unknown, never absent or disabled.
func observeService(ctx context.Context, binary, state string) serviceObservation {
	if !loginItemsSupported {
		return serviceObservation{state: serviceUnsupported, loaded: loadedUnknown}
	}
	obs := serviceObservation{state: serviceUnknown, loaded: loadedUnknown}
	disabled, known := readDisabled(ctx)
	obs.disabledKnown = known
	if known && disabled {
		obs.state = serviceDisabled
	}
	if out, ok := runObserve(ctx, "print", domain()+"/"+loginItemLabel); ok {
		if program, args, parsed := parseService(out); parsed {
			obs.program, obs.args = program, args
			if obs.state != serviceDisabled {
				obs.state = serviceLoaded
			}
			obs.loaded = loadedMismatch
			if program == binary && slices.Equal(args, []string{binary, "plugins", "supervise", "--state", state}) {
				obs.loaded = loadedMatches
			}
			return obs
		}
	}
	if out, ok := runObserve(ctx, "print", domain()); ok {
		if labels, complete := domainServices(out); complete && !slices.Contains(labels, loginItemLabel) {
			obs.absent = true
			if obs.state != serviceDisabled {
				obs.state = serviceNotLoaded
			}
		}
	}
	return obs
}

// readDisabled reads this label's disablement; known is false when the
// query fails or its list is not recognised.
func readDisabled(ctx context.Context) (disabled, known bool) {
	out, ok := runObserve(ctx, "print-disabled", domain())
	disabled, known = disabledState(out)
	known = ok && known
	return disabled, known
}

func runObserve(ctx context.Context, args ...string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, launchctlObserve)
	defer cancel()
	out, exit, err := runLaunchctl(ctx, args...)
	return out, err == nil && exit == 0
}

// disabledState reads `launchctl print-disabled`. Only the one top-level
// "disabled services" block counts; other blocks (newer systems add more)
// are skipped by depth. known needs that block complete and every row
// recognised; disabled when it marks this label so.
func disabledState(out []byte) (disabled, known bool) {
	depth, blocks, inList := 0, 0, false
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case inList:
			mine, ok := disabledRow(line)
			if !ok {
				return false, false
			}
			disabled = disabled || mine
			if line == "}" {
				inList, depth = false, depth-1
			}
		case strings.HasSuffix(line, "{"): // "name = {" or "name => {"
			if depth == 0 && line == "disabled services = {" {
				inList = true
				blocks++
			}
			depth++
		case line == "}" && depth > 0:
			depth--
		case depth == 0:
			return false, false // text outside every block, or an unmatched "}"
		}
	}
	return disabled, blocks == 1 && depth == 0 && !inList
}

// disabledRow reads one `"label" => value` row or the closing "}": mine is
// this label marked disabled.
func disabledRow(line string) (mine, ok bool) {
	if line == "}" {
		return false, true // the end of the list
	}
	label, value, found := strings.Cut(line, "=>")
	label, value = strings.TrimSpace(label), strings.TrimSpace(value)
	if !found || len(label) < 2 || label[0] != '"' || label[len(label)-1] != '"' {
		return false, false
	}
	switch value {
	case "disabled", "true":
		return label == `"`+loginItemLabel+`"`, true
	case "enabled", "false":
		return false, true
	}
	return false, false
}

// parseService reads the program and complete arguments of the job from
// `launchctl print gui/U/LABEL`. It follows the block depth: exactly one
// program and one arguments block at the top level, and the outer block
// must close on the last line. Anything else (truncated, duplicated or
// unknown framing) fails.
func parseService(out []byte) (string, []string, bool) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != domain()+"/"+loginItemLabel+" = {" {
		return "", nil, false
	}
	job := jobParse{depth: 1}
	for i, raw := range lines[1:] {
		if !job.line(strings.TrimSpace(raw), i == len(lines)-2) {
			return "", nil, false
		}
	}
	return job.program, job.args, job.depth == 0 && job.programs == 1 && job.argBlocks == 1 && job.program != ""
}

// jobParse is parseService's state: the block depth and what the top level
// of the job has said.
type jobParse struct {
	program             string
	args                []string
	programs, argBlocks int
	depth               int
	inArgs              bool
}

// line reads one line; false means the framing is not recognised.
func (j *jobParse) line(line string, last bool) bool {
	switch {
	case line == "}":
		j.depth--
		j.inArgs = false
		return j.depth > 0 || last // no text after the outer block
	case j.inArgs:
		j.args = append(j.args, line)
	case strings.HasSuffix(line, "= {"):
		if j.depth == 1 && line == "arguments = {" {
			j.inArgs = true
			j.argBlocks++
		}
		j.depth++
	case j.depth == 1 && strings.HasPrefix(line, "program = "):
		j.program = strings.TrimPrefix(line, "program = ")
		j.programs++
	}
	return true
}

// domainServices lists the labels of `launchctl print gui/U`. complete is
// true only for the recognised framing: the gui/U header, exactly one
// top-level services block whose rows all read as serviceRow, and the outer
// block closing on the last line. Every other block is skipped by depth.
func domainServices(out []byte) ([]string, bool) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != domain()+" = {" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return nil, false
	}
	d := domainParse{depth: 1}
	for _, raw := range lines[1 : len(lines)-1] {
		if !d.line(strings.TrimSpace(raw)) {
			return nil, false
		}
	}
	return d.labels, d.depth == 1 && d.blocks == 1 && !d.inServices
}

// domainParse is domainServices' state: block depth, the one top-level
// services block and its labels.
type domainParse struct {
	labels        []string
	depth, blocks int
	inServices    bool
}

// line reads one line; false means the framing is not recognised.
func (d *domainParse) line(line string) bool {
	switch {
	case d.inServices && line == "}":
		d.inServices, d.depth = false, d.depth-1
	case d.inServices:
		row := serviceRow.FindStringSubmatch(line)
		if row == nil {
			return false
		}
		d.labels = append(d.labels, row[1])
	case line == "}":
		d.depth--
		return d.depth >= 1 // the outer block closed early
	case strings.HasSuffix(line, "{"): // "name = {" or "name => {"
		if d.depth == 1 && line == "services = {" {
			d.inServices = true
			d.blocks++
		}
		d.depth++
	}
	return true
}

// serviceRow is one row of the services block: the PID or "-", the last
// exit status ("-", a number, or a parenthesised word such as "(pe)" on
// current macOS), then the label (the rest of the line).
var serviceRow = regexp.MustCompile(`^(?:-|[0-9]+)\s+(?:-|-?[0-9]+|\([a-z]+\))\s+(\S.*)$`)

// runMutationKnown runs one bounded launchctl change. known is false when
// there is no exit status (start failure, timeout): the change may or may
// not have happened.
func runMutationKnown(ctx context.Context, args ...string) (known bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, loginItemOpLimit)
	defer cancel()
	_, exit, err := runLaunchctl(ctx, args...)
	switch {
	case err != nil:
		return false, err
	case exit != 0:
		return true, errors.New("launchctl " + args[0] + " exited " + strconv.Itoa(exit))
	}
	return true, nil
}
