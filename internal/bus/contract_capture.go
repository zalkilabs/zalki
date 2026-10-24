package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Capture is opt-in and allowlisted: no prompts, tool input, environment,
// credentials, native IDs, titles, or workstation paths reach the fixture.
func captureHookContract(adapter Adapter, event HookInput) error {
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
	hook := map[string]any{"hook_event_name": event.Event}
	if event.SessionID != "" {
		hook["session_id"] = "00000000-0000-4000-8000-000000000001"
	}
	if event.ConversationID != "" {
		hook["conversation_id"] = "00000000-0000-4000-8000-000000000001"
	}
	if event.CWD != "" {
		hook["cwd"] = "/fixture/workspace"
	}
	if len(event.WorkspaceRoots) > 0 {
		hook["workspace_roots"] = []string{"/fixture/workspace"}
	}
	switch event.Source {
	case "startup", "resume", "compact":
		hook["source"] = event.Source
	}
	version := "unknown"
	if path, err := execClientPath(adapter.Descriptor().Executable); err == nil {
		if value := probeClientVersion(path); value != "" {
			version = value
		}
	}
	fixture := map[string]any{"kind": kind, "version": version, "version_output": version, "hook": hook,
		"provenance": map[string]any{"source": "native_hook", "normalized": true, "captured_at": timestamp(), "scope": "startup hook fields only; no live delivery claim"}}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return err
	}
	err = exclusiveFile(filepath.Join(root, kind+"-"+version+".json"), append(encoded, '\n'))
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}
