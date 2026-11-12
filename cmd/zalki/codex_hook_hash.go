package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Pinned to openai/codex rust-v0.167.2, hooks/src/engine/discovery.rs
// hook_hash/append_matcher_groups and config/src/fingerprint.rs. Admission of
// another version requires native fixtures; this is not a general hash API.
var codexEventKeys = map[string]string{
	"PreToolUse": "pre_tool_use", "PermissionRequest": "permission_request",
	"PostToolUse": "post_tool_use", "PreCompact": "pre_compact", "PostCompact": "post_compact",
	"PreModelRequest": "pre_model_request", "PostModelRequest": "post_model_request",
	"SessionStart": "session_start", "SessionEnd": "session_end", "UserPromptSubmit": "user_prompt_submit",
	"SubagentStart": "subagent_start", "SubagentStop": "subagent_stop", "Stop": "stop", "Interrupt": "interrupt",
}

var errCodexDefinition = errors.New("unsupported Codex hook definition")

func codexHookHash(event string, matcher any, handler map[string]any) (string, error) {
	identity, err := codexHookIdentity(event, matcher, handler)
	if err != nil {
		return "", err
	}
	raw, err := codexCanonicalJSON(nil, identity)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}

func codexHookIdentity(event string, matcher any, handler map[string]any) (map[string]any, error) {
	key, ok := codexEventKeys[event]
	if !ok || !codexKnownFields(handler, "type", "command", "commandWindows", "command_windows", "timeout", "async", "statusMessage", "additionalContextLimit") {
		return nil, errCodexDefinition
	}
	command, ok := handler["command"].(string)
	if !ok || strings.TrimSpace(command) == "" || handler["type"] != "command" {
		return nil, errCodexDefinition
	}
	timeout, err := codexHookTimeout(event, handler["timeout"])
	if err != nil {
		return nil, err
	}
	async := false
	if value, exists := handler["async"]; exists {
		async, ok = value.(bool)
		if !ok {
			return nil, errCodexDefinition
		}
	}
	normal := map[string]any{"type": "command", "command": command, "timeout": timeout, "async": async}
	if err := codexOptionalHandlerFields(event, handler, normal); err != nil {
		return nil, err
	}
	identity := map[string]any{"event_name": key, "hooks": []any{normal}}
	if matcher != nil {
		value, valid := matcher.(string)
		if !valid {
			return nil, errCodexDefinition
		}
		if codexRetainsMatcher(event) {
			identity["matcher"] = value
		}
	}
	return identity, nil
}

func codexHookTimeout(event string, value any) (int64, error) {
	short := event == "SessionEnd" || event == "Interrupt"
	timeout := int64(600)
	if short {
		timeout = 1
	}
	if value != nil {
		var err error
		timeout, err = codexUnsignedInteger(value)
		if err != nil {
			return 0, err
		}
	}
	if timeout < 1 {
		timeout = 1
	}
	if short && timeout > 3 {
		timeout = 3
	}
	return timeout, nil
}

func codexOptionalHandlerFields(event string, handler, normal map[string]any) error {
	for _, field := range []string{"statusMessage", "commandWindows", "command_windows"} {
		if value := handler[field]; value != nil {
			if _, ok := value.(string); !ok {
				return errCodexDefinition
			}
			if field == "statusMessage" {
				normal[field] = value
			}
		}
	}
	if _, camel := handler["commandWindows"]; camel {
		if _, snake := handler["command_windows"]; snake {
			return errCodexDefinition
		}
	}
	if value := handler["additionalContextLimit"]; value != nil {
		limit, err := codexUnsignedInteger(value)
		if err != nil {
			return err
		}
		if codexSupportsContext(event) && limit != 2500 {
			normal["additionalContextLimit"] = limit
		}
	}
	return nil
}

func codexRetainsMatcher(event string) bool {
	switch event {
	case "PreToolUse", "PermissionRequest", "PostToolUse", "SessionStart", "SessionEnd", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact":
		return true
	}
	return false
}

func codexSupportsContext(event string) bool {
	switch event {
	case "PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "SubagentStart":
		return true
	}
	return false
}

func codexUnsignedInteger(value any) (int64, error) {
	var integer int64
	var err error
	switch value := value.(type) {
	case json.Number:
		integer, err = value.Int64()
	case int64:
		integer = value
	case int:
		integer = int64(value)
	default:
		return 0, errCodexDefinition
	}
	if err != nil || integer < 0 {
		return 0, errCodexDefinition
	}
	return integer, nil
}

func codexKnownFields(object map[string]any, fields ...string) bool {
	for key := range object {
		if !containsCodexField(fields, key) {
			return false
		}
	}
	return true
}

func containsCodexField(fields []string, key string) bool {
	for _, field := range fields {
		if key == field {
			return true
		}
	}
	return false
}

// serde_json escapes controls, quotes and backslashes, but leaves HTML and
// U+2028/U+2029 as UTF-8. Go's encoding/json also escapes the latter even with
// SetEscapeHTML(false). Encode strings directly, without replacing escapes.
func codexCanonicalString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			dst = append(dst, '\\', byte(r))
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if r < 0x20 {
				const digits = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', digits[r>>4], digits[r&15])
			} else {
				dst = append(dst, string(r)...)
			}
		}
	}
	return append(dst, '"')
}

func codexCanonicalJSON(dst []byte, value any) ([]byte, error) {
	switch value := value.(type) {
	case string:
		return codexCanonicalString(dst, value), nil
	case bool:
		return strconv.AppendBool(dst, value), nil
	case int64:
		return strconv.AppendInt(dst, value, 10), nil
	case []any:
		dst = append(dst, '[')
		for i, item := range value {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = codexCanonicalJSON(dst, item)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		return codexCanonicalObject(dst, value)
	}
	return nil, errCodexDefinition
}

func codexCanonicalObject(dst []byte, object map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	dst = append(dst, '{')
	for i, key := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(codexCanonicalString(dst, key), ':')
		var err error
		dst, err = codexCanonicalJSON(dst, object[key])
		if err != nil {
			return nil, err
		}
	}
	return append(dst, '}'), nil
}
