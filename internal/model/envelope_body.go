package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	envelopeDisplayBytes = 16 * 1024
	envelopeJSONBytes    = 32 * 1024
	displayShortened     = "\n[Display shortened; read the full body by message ID.]"
)

var bodyMarker = regexp.MustCompile(`(?i)\[\s*(?:/\s*zalki|zalki\s+(?:message|notice))`)

// envelopeBody escapes display controls and marker delimiters without changing storage.
func envelopeBody(body string) string { return envelopeBodyBudget(body, envelopeJSONBytes) }

func envelopeBodyBudget(body string, jsonBudget int) string {
	escaped := escapeBodyControls(body)
	if len(escaped) <= envelopeDisplayBytes && jsonTextBytes(escaped) <= jsonBudget {
		return escaped
	}
	return boundedEnvelopeBody(escaped, displayShortened, jsonBudget)
}

// markerStarts folds only a detection copy. It maps each recognized marker's
// delimiter and ignorable runes back to their original byte offsets.
func markerStarts(body string) map[int]bool {
	var folded strings.Builder
	offsets := make([]int, 0, len(body))
	for i, r := range body {
		if markerIgnorable(r) {
			continue
		}
		if r >= '\uFF01' && r <= '\uFF5E' {
			r -= 0xFEE0
		}
		if unicode.IsSpace(r) {
			r = ' '
		}
		for range utf8.RuneLen(r) {
			offsets = append(offsets, i)
		}
		folded.WriteRune(r)
	}
	starts := make(map[int]bool)
	for _, match := range bodyMarker.FindAllStringIndex(folded.String(), -1) {
		start, end := offsets[match[0]], len(body)
		if match[1] < len(offsets) {
			end = offsets[match[1]]
		}
		starts[start] = true
		for i, r := range body[start:end] {
			if markerIgnorable(r) {
				starts[start+i] = true
			}
		}
	}
	return starts
}

// Other_Default_Ignorable_Code_Point includes fillers and reserved tag ranges
// outside Cf and marks. Ignore these only when recognizing marker framing;
// ordinary accents and emoji selectors outside a marker stay readable.
func markerIgnorable(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me, unicode.Other_Default_Ignorable_Code_Point)
}

func escapeBodyControls(body string) string {
	markers := markerStarts(body)
	var out strings.Builder
	for i, r := range body {
		if markers[i] || r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029') {
			if r <= 0xFFFF {
				fmt.Fprintf(&out, `\u%04X`, r)
			} else {
				fmt.Fprintf(&out, `\U%08X`, r)
			}
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func boundedEnvelopeBody(body, suffix string, jsonBudget int) string {
	var out strings.Builder
	cost := jsonTextBytes(suffix)
	for body != "" {
		unit := displayUnit(body)
		unitCost := jsonTextBytes(unit)
		if out.Len()+len(unit)+len(suffix) > envelopeDisplayBytes || cost+unitCost > jsonBudget {
			break
		}
		out.WriteString(unit)
		cost += unitCost
		body = body[len(unit):]
	}
	return out.String() + suffix
}

func displayUnit(body string) string {
	for _, escape := range []struct {
		prefix string
		width  int
	}{{`\u`, 6}, {`\U`, 10}} {
		if len(body) >= escape.width && strings.HasPrefix(body, escape.prefix) && strings.Trim(body[2:escape.width], "0123456789abcdefABCDEF") == "" {
			return body[:escape.width]
		}
	}
	_, size := utf8.DecodeRuneInString(body)
	return body[:size]
}

func jsonTextBytes(text string) int {
	encoded, _ := json.Marshal(text)
	return len(encoded) - 2
}
