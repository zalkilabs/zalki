package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

const restartRequestLimit = 1 << 10

// restartRequest asks the controller to clear one instance's crash history
// and start it again. It names the installation and binding it was made
// for; it carries no executable, path, grant or PID.
type restartRequest struct {
	Installation string    `json:"installation"`
	Epoch        string    `json:"epoch,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	Seq          uint64    `json:"seq"`
	Nonce        string    `json:"nonce"`
	RequestedAt  time.Time `json:"requested_at"`
}

func restartPath(state, name string) string {
	return filepath.Join(state, "plugins", name+".restart.json")
}

// withRestartLock holds NAME.restart.lock, waiting at most two seconds.
// All issuers and the controller use it; it is separate from the Store.
func withRestartLock(state, name string, fn func() error) error {
	path := filepath.Join(state, "plugins", name+".restart.lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: fixed path under the state directory; links refused.
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // Closing releases the lock.
	if info, err := file.Stat(); err != nil || !privateFile(info) {
		return unsafeLog(path, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return model.NewError(model.CodeUnavailable, "the restart request for %s is busy; try again", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// loadRestartRequest reads a pending request strictly; ok is false when
// there is none.
func loadRestartRequest(state, name string) (restartRequest, bool, error) {
	path := restartPath(state, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return restartRequest{}, false, nil
	}
	if err != nil {
		return restartRequest{}, false, err
	}
	var request restartRequest
	raw, err := journal.ReadBytes(path, restartRequestLimit)
	if err == nil && privateFile(info) {
		err = bus.DecodeStrictRecordJSON(raw, &request)
	} else if err == nil {
		err = errors.New("not private")
	}
	if err != nil || !model.IsCanonicalUUID(request.Installation) || request.Seq == 0 || !model.IsCanonicalUUID(request.Nonce) || request.RequestedAt.IsZero() || !canonicalJSON(raw, request) {
		return restartRequest{}, false, model.NewError(model.CodeConflict, "%s is damaged; it was kept", path)
	}
	return request, true, nil
}

// requestRestart writes the next request for the current binding. The
// sequence is above both a pending request and the consumed high-water
// mark, so an old request can never reset later failures.
func requestRestart(state, name string, status pluginfeed.PluginFeedStatus, now time.Time) error {
	return withRestartLock(state, name, func() error {
		run, err := loadRunState(state, name, status.ID)
		if err != nil {
			return err
		}
		pending, _, err := loadRestartRequest(state, name)
		if err != nil {
			return err
		}
		high := max(pending.Seq, run.RestartSeq)
		if high == math.MaxUint64 {
			return model.NewError(model.CodeConflict, "restart sequence for %s is exhausted", name)
		}
		raw, err := json.Marshal(restartRequest{Installation: status.ID, Epoch: status.Epoch, Revision: status.Revision, Seq: high + 1, Nonce: model.NewUUID(), RequestedAt: now.UTC()})
		if err != nil {
			return err
		}
		return journal.WriteAtomic(restartPath(state, name), raw)
	})
}

// consumeRestart applies a pending request for the current binding. The
// reset and the new high-water mark are ONE durable run-state write; only
// after it is the request removed. A request at or below the mark is only
// cleaned up; one for another installation or binding is ignored.
func (s *supervisor) consumeRestart(name string, p *supervised, key bindingKey) bool {
	applied := false
	err := withRestartLock(s.state, name, func() error {
		request, ok, err := loadRestartRequest(s.state, name)
		if err != nil || !ok {
			return err
		}
		switch {
		case request.Seq <= p.run.RestartSeq && p.unsaved:
			// The mark covering it is not durable yet: keep the request
			// until a save confirms the mark, or a crash could lose both.
			return nil
		case request.Seq <= p.run.RestartSeq:
			// Already consumed before a crash: cleanup only.
		case request.Installation != key.id || request.Epoch != key.epoch || request.Revision != key.revision:
			s.logf("%s: restart request ignored: stale_binding", name)
		default:
			next := p.run
			next.RestartSeq, next.Restarts, next.BackoffStep, next.NextRetryAt = request.Seq, nil, 0, nil
			if next.State == runCrashed || next.State == runBackoff {
				next.State = runStopped
			}
			next.setReason(s.now(), "restart_requested")
			if err := s.saveRun(s.state, name, next); err != nil {
				// The write may have been published (an error after the
				// rename): keep the new mark in memory and hold every start
				// until a save confirms it. The request stays; its replay
				// is then cleanup only.
				p.run, p.unsaved = next, true
				return err
			}
			p.run, applied = next, true
		}
		if err := os.Remove(restartPath(s.state, name)); err != nil {
			return err
		}
		return journal.SyncDir(filepath.Join(s.state, "plugins"))
	})
	if err != nil {
		s.logf("%s: restart request not applied: %s", name, model.ErrorCodeOf(err))
	}
	return applied
}

func newPluginRestartCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "restart NAME", Short: "Ask the supervisor to start a plugin again, clearing its crash limit", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			name, state := args[0], service.Store.Dir
			status, err := pluginfeed.NewPluginFeed(service.Store, name).PluginStatus()
			if err != nil {
				return err
			}
			if status.ID == "" || !status.Enabled {
				return model.NewError(model.CodeInvalidTransition, "%s is disabled; enable it before requesting a restart", name)
			}
			if err := requestRestart(state, name, status, time.Now()); err != nil {
				return err
			}
			if supervisorRunning(state) {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Restart requested for %s.\n", name)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Restart requested for %s. Run zalki plugins supervise --state %s to start it.\n", name, shellQuote(state))
			return err
		},
	}
}

// supervisorRunning reports a live controller: its singleton lock is held.
func supervisorRunning(state string) bool {
	file, err := os.OpenFile(filepath.Join(state, "plugins", ".supervisor.lock"), os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: fixed path under the state directory.
	if err != nil {
		return false
	}
	defer file.Close() //nolint:errcheck // Read-only probe.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	return false
}

// shellQuote returns value as one POSIX shell word, so a printed command
// works when pasted, whatever the path holds.
func shellQuote(value string) string {
	if value != "" && !strings.ContainsFunc(value, shellUnsafeRune) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// shellUnsafeRune: any character outside letters, digits and /._-+@:,=.
func shellUnsafeRune(r rune) bool {
	safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+@:,=", r)
	return !safe
}
