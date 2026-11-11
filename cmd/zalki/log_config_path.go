package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

func sameLogConfigFile(a, b string) (bool, error) {
	left, leftErr := os.Stat(a)
	right, rightErr := os.Stat(b)
	for _, err := range []error{leftErr, rightErr} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if leftErr == nil && rightErr == nil && os.SameFile(left, right) {
		return true, nil
	}
	leftPath, err := resolvedLogPath(a)
	if err != nil {
		return false, err
	}
	rightPath, err := resolvedLogPath(b)
	return leftPath == rightPath, err
}

// Resolve the existing parent of a future target, retaining its missing suffix.
func resolvedLogPath(path string) (string, error) {
	path = filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(path) == path {
			return "", err
		}
		suffix = append(suffix, filepath.Base(path))
		path = filepath.Dir(path)
	}
}

// Version recovery can lack a snapshot; its flag/env sink still obeys the selector rule.
func absoluteLogPath(path string) (string, error) {
	if path == "-" {
		return path, nil
	}
	if !utf8.ValidString(path) || strings.IndexFunc(path, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return "", errors.New("invalid log path. Use a nonempty path without control characters (including newlines) and an absolute path or valid absolute cwd")
	}
	return filepath.Abs(path)
}

func checkLogConfigFile(path, configFile string) error {
	if path == "-" {
		return nil
	}
	same, err := sameLogConfigFile(path, configFile)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("--log-file points at the selected config file (%q); choose another log file", configFile)
	}
	return nil
}
