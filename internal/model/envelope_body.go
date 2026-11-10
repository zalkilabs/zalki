package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	envelopeDisplayBytes = 16 * 1024
	envelopeJSONBytes    = 32 * 1024
	displayShortened     = "\n[Display shortened; read the full body by message ID.]"
)

var bodyMarkers = strings.NewReplacer(
	"[/zalki]", `\u005B/zalki]`,
	"[zalki message", `\u005Bzalki message`,
	"[zalki notice", `\u005Bzalki notice`,
)

// V1 visibly escapes body markers and display controls, preserving ordinary text and raw storage.
func envelopeBody(body string) string {
	escaped := bodyMarkers.Replace(escapeBodyControls(body))
	if escaped == body || len(escaped) <= envelopeDisplayBytes && jsonTextBytes(escaped) <= envelopeJSONBytes {
		return escaped
	}
	return boundedEnvelopeBody(escaped, displayShortened)
}

func escapeBodyControls(body string) string {
	var out strings.Builder
	for _, r := range body {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)) {
			fmt.Fprintf(&out, `\u%04X`, r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func boundedEnvelopeBody(body, suffix string) string {
	var out strings.Builder
	cost := jsonTextBytes(suffix)
	for body != "" {
		unit := displayUnit(body)
		unitCost := jsonTextBytes(unit)
		if out.Len()+len(unit)+len(suffix) > envelopeDisplayBytes || cost+unitCost > envelopeJSONBytes {
			break
		}
		out.WriteString(unit)
		cost += unitCost
		body = body[len(unit):]
	}
	return out.String() + suffix
}

func displayUnit(body string) string {
	for _, prefix := range []string{`\u005B/zalki]`, `\u005Bzalki message`, `\u005Bzalki notice`} {
		if strings.HasPrefix(body, prefix) {
			line, _, _ := strings.Cut(body, "\n")
			if end := strings.IndexByte(line, ']'); end >= 0 {
				return body[:end+1]
			}
			return body[:len(prefix)]
		}
	}
	if len(body) >= 6 && body[:2] == `\u` && strings.Trim(body[2:6], "0123456789abcdefABCDEF") == "" {
		return body[:6]
	}
	_, size := utf8.DecodeRuneInString(body)
	return body[:size]
}

func jsonTextBytes(text string) int {
	encoded, _ := json.Marshal(text)
	return len(encoded) - 2
}
