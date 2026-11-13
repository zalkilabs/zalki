package main

import (
	"strconv"
	"strings"
	"unicode"
)

const installQuestion = pluginReviewQuestion + " "

// plainText shows owner-facing text safely: anything not printable is quoted.
func plainText(value string) string {
	if strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return strconv.Quote(value)
	}
	return value
}

// plainLines keeps real line breaks and makes each line safe on its own.
func plainLines(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = plainText(line)
	}
	return strings.Join(lines, "\n")
}
