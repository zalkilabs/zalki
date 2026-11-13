package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

type pluginReviewAnswer uint8

const (
	pluginReviewInvalid pluginReviewAnswer = iota
	pluginReviewCancel
	pluginReviewAccept
)

var (
	errPluginReviewInput   = errors.New("could not read plugin install input")
	errPluginReviewTooLong = errors.New("plugin install input is too long")
)

// readPluginReviewAnswer interprets bytes only. Its caller must verify a real
// terminal, write the question successfully, own signal/termios restoration and
// use one shared buffered reader across selector and confirmation operations.
// An invalid answer is not approval; the controller permits at most 3 attempts.
func readPluginReviewAnswer(in *bufio.Reader, raw bool) (pluginReviewAnswer, error) {
	if in == nil {
		return pluginReviewCancel, errPluginReviewInput
	}
	if raw {
		return readPluginReviewRaw(in)
	}
	line, complete, err := readPluginReviewLine(in, 256)
	if err != nil || !complete {
		return pluginReviewCancel, err
	}
	switch strings.ToLower(strings.Trim(line, " \t\r")) {
	case "y", "yes":
		return pluginReviewAccept, nil
	case "", "n", "no", "c", "cancel", "q":
		return pluginReviewCancel, nil
	default:
		return pluginReviewInvalid, nil
	}
}

func readPluginReviewRaw(in *bufio.Reader) (pluginReviewAnswer, error) {
	key, err := in.ReadByte()
	if errors.Is(err, io.EOF) {
		return pluginReviewCancel, nil
	}
	if err != nil {
		return pluginReviewCancel, errPluginReviewInput
	}
	switch key {
	case 'y', 'Y':
		return pluginReviewAccept, nil
	case '\n', '\r', 'n', 'N', 'c', 'C', 'q', 'Q', 3, 4, 0x1b:
		return pluginReviewCancel, nil
	default:
		return pluginReviewInvalid, nil
	}
}

// readPluginReviewLine requires a complete LF-terminated line. EOF discards any
// partial answer; overlong input aborts rather than draining an unbounded stream.
// Ctrl-C/Ctrl-D/ESC cancel without treating following buffered bytes as consent.
func readPluginReviewLine(in *bufio.Reader, limit int) (string, bool, error) {
	if in == nil || limit < 1 || limit > 4096 {
		return "", false, errPluginReviewInput
	}
	line := make([]byte, 0, min(limit, 256))
	for {
		key, err := in.ReadByte()
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, errPluginReviewInput
		}
		if key == 3 || key == 4 || key == 0x1b {
			return "", false, nil
		}
		if key == '\n' {
			return string(line), true, nil
		}
		if len(line) == limit {
			return "", false, errPluginReviewTooLong
		}
		line = append(line, key)
	}
}
