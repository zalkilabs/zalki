package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

type fileSettings struct {
	Codex struct {
		AllowQueueFallback *bool `json:"allow_queue_fallback"`
	} `json:"codex"`
	Messages struct {
		MaxBodyBytes *int `json:"max_body_bytes"`
	} `json:"messages"`
	Logging struct {
		Level *string `json:"level"`
		File  *string `json:"file"`
	} `json:"logging"`
	Prune struct {
		RetiredOlderThan *string `json:"retired_older_than"`
	} `json:"prune"`
}

var objectKeys = map[string][]string{
	"":         {"codex", "logging", "messages", "prune"},
	"codex":    {"allow_queue_fallback"},
	"logging":  {"file", "level"},
	"messages": {"max_body_bytes"},
	"prune":    {"retired_older_than"},
}

type fileContent struct {
	path      string
	raw       []byte
	locations map[string]int
}

func decodeConfig(content fileContent) (fileSettings, error) {
	var settings fileSettings
	scanner := json.NewDecoder(bytes.NewReader(content.raw))
	if err := content.scanObject(scanner, ""); err != nil {
		return settings, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content.raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		var typed *json.UnmarshalTypeError
		if errors.As(err, &typed) {
			return settings, content.atKey(typed.Field, "wrong JSON type", typeFix(typed.Field))
		}
		return settings, content.syntax(err)
	}
	end := decoder.InputOffset()
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return settings, content.at(int(end), "", "extra content follows the JSON object", "Remove all content after the first JSON object")
	}
	return settings, nil
}

func (content fileContent) scanObject(decoder *json.Decoder, parent string) error {
	start, err := decoder.Token()
	if err != nil {
		return content.syntax(err)
	}
	if start != json.Delim('{') {
		return content.atKey(parent, "expected a JSON object", "Use a JSON object for this key")
	}
	seen := map[string]bool{}
	for decoder.More() {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return content.syntax(err)
		}
		key, ok := token.(string)
		if !ok {
			return content.at(int(before), parent, "expected an object key", "Correct the JSON syntax at this line")
		}
		path := key
		if parent != "" {
			path = parent + "." + key
		}
		offset := int(before) + bytes.IndexByte(content.raw[before:decoder.InputOffset()], '"')
		content.locations[path] = offset
		if err := content.checkObjectKey(parent, path, key, seen); err != nil {
			return err
		}
		if err := content.scanValue(decoder, path); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	if err != nil {
		return content.syntax(err)
	}
	return nil
}

func (content fileContent) checkObjectKey(parent, path, key string, seen map[string]bool) error {
	if protectedKey(key) {
		return errOwnerOnly
	}
	if seen[key] {
		return content.atKey(path, "duplicate key", "Remove the duplicate key so each key appears once")
	}
	seen[key] = true
	if !slices.Contains(objectKeys[parent], key) {
		return content.atKey(path, "unknown key", "Remove it or check the spelling. Allowed keys are "+strings.Join(objectKeys[parent], ", "))
	}
	return nil
}

func (content fileContent) scanValue(decoder *json.Decoder, path string) error {
	if _, object := objectKeys[path]; object {
		return content.scanObject(decoder, path)
	}
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return content.syntax(err)
	}
	if bytes.Equal(value, []byte("null")) {
		return content.atKey(path, "null is not a setting value", typeFix(path))
	}
	return nil
}

func typeFix(key string) string {
	if key == "codex.allow_queue_fallback" {
		return "Use true or false for this key, or remove the optional key to inherit its default"
	}
	if key == "messages.max_body_bytes" {
		return "Use an integer for this key, or remove the optional key to inherit its default"
	}
	return "Use a string for this key, or remove the optional key to inherit its default"
}

func (content fileContent) atKey(key, problem, fix string) error {
	return content.at(content.locations[key], key, problem, fix)
}

func (content fileContent) at(offset int, key, problem, fix string) error {
	offset = max(0, min(offset, len(content.raw)))
	line := 1 + bytes.Count(content.raw[:offset], []byte{'\n'})
	if key != "" {
		return fmt.Errorf("%s line %d, key %q: %s. %s", content.path, line, key, problem, fix)
	}
	return fmt.Errorf("%s line %d: %s. %s", content.path, line, problem, fix)
}

func (content fileContent) syntax(err error) error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return content.at(int(syntax.Offset)-1, "", "invalid JSON", "Correct the JSON syntax at this line")
	}
	return content.at(len(content.raw), "", "incomplete JSON object", "Correct the JSON syntax at this line")
}
