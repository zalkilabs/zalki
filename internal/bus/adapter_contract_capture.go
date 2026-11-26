package bus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/fsutil"

	"github.com/zalkilabs/zalki/internal/model"
)

// Capture is opt-in and allowlisted: no prompts, tool input, environment,
// credentials, native IDs, titles, or workstation paths reach the fixture.
func captureHookContract(ctx context.Context, adapter agent.Adapter, event agent.HookInput) error {
	root := os.Getenv("ZALKI_CAPTURE_CONTRACTS")
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return errors.New("ZALKI_CAPTURE_CONTRACTS must be an absolute private directory")
	}
	kind := adapter.Descriptor().Kind
	if adapter.Descriptor().Hooks[event.Event] != "SessionStart" {
		return nil
	}
	if err := privateDir(root); err != nil {
		return err
	}
	hook := map[string]any{adapter.Descriptor().HookEventKey: event.Event}
	if event.SessionID != "" {
		hook["session_id"] = "00000000-0000-4000-8000-000000000001"
	}
	if event.CWD != "" {
		hook["cwd"] = "/fixture/workspace"
	}
	captureNative(hook, adapter.Descriptor().Native, event.Native)
	switch event.Source {
	case "startup", "resume", "compact":
		hook["source"] = event.Source
	}
	version := "unknown"
	if path, err := execClientPath(adapter.Descriptor().Executable); err == nil {
		if value := probeClientVersion(ctx, path); value != "" {
			version = value
		}
	}
	fixture := map[string]any{"kind": kind, "version": version, "version_output": version, "hook": hook,
		"provenance": map[string]any{"source": "native_hook", "normalized": true, "captured_at": model.Timestamp(), "scope": "startup hook fields only; no live delivery claim"}}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return err
	}
	err = fsutil.WriteExclusive(filepath.Join(root, kind+"-"+version+".json"), append(encoded, '\n'))
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

// captureNative records each declared native field the event carried, with a
// placeholder: no native value reaches the fixture.
func captureNative(hook map[string]any, fields []agent.NativeField, native json.RawMessage) {
	var values map[string]json.RawMessage
	if json.Unmarshal(native, &values) != nil {
		return
	}
	for _, field := range fields {
		value, ok := values[field.Key]
		switch {
		case !ok:
		case field.Capture == agent.CaptureID && string(value) != `""`:
			hook[field.Key] = "00000000-0000-4000-8000-000000000001"
		case field.Capture == agent.CapturePaths && string(value) != "[]":
			hook[field.Key] = []string{"/fixture/workspace"}
		}
	}
}
