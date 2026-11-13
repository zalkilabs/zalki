package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type pluginStartupFormat func(JournalView, string, model.Session, bool, func(string) (bool, error)) (string, error)

const pluginStartupUnavailable = "Plugins active for this session: unavailable. Run: zalki plugins list"
const startupTooLarge = "Zalki setup instructions are too large. Shorten the configured paths, then start a new chat."

var errPluginStartupSize = errors.New(startupTooLarge)

func currentPluginStartup(view JournalView, s Store, current model.Session, format pluginStartupFormat, reserve bool, fits func(string) (bool, error)) string {
	if _, valid := decodeSessionPluginPolicy(current); !valid {
		current.PluginPolicy = nil
	}
	if format == nil {
		return pluginStartupUnavailable
	}
	text, err := format(view, s.Dir, current, reserve, fits)
	if err != nil {
		s.logEvent(slog.LevelWarn, "plugin.startup_unavailable", eventData{Actor: current.ID})
	}
	if text == "" {
		return pluginStartupUnavailable
	}
	return text
}

func (s *Service) sessionStartupOutput(hooks agent.HookAdapter, current model.Session, key string, reserve bool, output io.Writer, traces ...*hookTrace) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	startup := agent.StartupContext{Actor: current.ID, Capability: key, StateDir: s.Store.Dir, ConfigFile: s.Store.ConfigFile, Instructions: startupInstructions(current.ID, key), Executable: binary}
	base, err := encodeStartupOutput(hooks, startup)
	if err != nil {
		return err
	}
	var selected []byte
	err = WithJournalWrite(s.Store, func(view JournalView) error {
		fresh, err := sessionForHook(s.Store, current.ID, hookRuntime{actor: current.ID, kind: current.Kind, generation: current.Generation, generationBound: true})
		if err != nil {
			return err
		}
		fits := func(text string) (bool, error) {
			with := startup
			with.Instructions += "\n" + text
			raw, err := encodeStartupOutput(hooks, with)
			if err != nil {
				return false, err
			}
			if len(text) > 2048 || len(raw)-len(base) > 2048 || len(raw) > hookContextBytes {
				return false, nil
			}
			selected = raw
			return true, nil
		}
		text := currentPluginStartup(view, s.Store, fresh, s.PluginStartup, reserve, fits)
		ok, err := fits(text)
		if err != nil {
			return err
		}
		if !ok {
			return errPluginStartupSize
		}
		return nil
	})
	if errors.Is(err, errPluginStartupSize) {
		_, _ = fmt.Fprintln(os.Stderr, startupTooLarge)
		return nil
	}
	if err != nil {
		return err
	}
	if len(traces) > 0 {
		traces[0].stage = "output"
	}
	n, err := output.Write(selected)
	if err == nil && n != len(selected) {
		return io.ErrShortWrite
	}
	return err
}

func encodeStartupOutput(hooks agent.HookAdapter, startup agent.StartupContext) ([]byte, error) {
	result, err := hooks.StartupOutput(startup)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	return append(raw, '\n'), err
}
