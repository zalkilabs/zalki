package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

func installationBinary() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return invokedBinary(os.Args[0], executable)
}

// Preserve the invoked symlink so hooks survive package upgrades.
func invokedBinary(invoked, executable string) (string, error) {
	path, err := exec.LookPath(invoked)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	a, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	b, err := os.Stat(executable)
	if err != nil {
		return "", err
	}
	if !os.SameFile(a, b) {
		return "", errors.New("invoked binary no longer matches this executable; run the installed zalki again")
	}
	return path, nil
}
