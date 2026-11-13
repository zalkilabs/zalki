package bus

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The standard decoder accepts duplicate object keys and null scalar fields.
// Policy is an authority record, so reject those before typed decoding.
func sessionPluginUniqueJSON(raw []byte) bool {
	if !json.Valid(raw) {
		return false
	}
	return consumeSessionPluginJSON(json.NewDecoder(bytes.NewReader(raw)), 0)
}

func consumeSessionPluginJSON(d *json.Decoder, depth int) bool {
	if depth > 8 {
		return false
	}
	token, err := d.Token()
	if err != nil || token == nil {
		return false
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return true
	}
	if delimiter == '{' {
		return consumeSessionPluginObject(d, depth+1)
	}
	if delimiter != '[' {
		return false
	}
	for d.More() {
		if !consumeSessionPluginJSON(d, depth+1) {
			return false
		}
	}
	end, err := d.Token()
	return err == nil && end == json.Delim(']')
}

func consumeSessionPluginObject(d *json.Decoder, depth int) bool {
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || key != strings.ToLower(key) {
			return false
		}
		seen[key] = true
		if !consumeSessionPluginJSON(d, depth) {
			return false
		}
	}
	end, err := d.Token()
	return err == nil && end == json.Delim('}')
}
