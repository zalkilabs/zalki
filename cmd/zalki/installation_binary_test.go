package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallationRetainsStableBinarySymlink(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, "bin", "zalki")
	if err := os.MkdirAll(filepath.Dir(stable), 0700); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1", "v2"} {
		target := filepath.Join(root, version, "zalki")
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if version == "v2" {
			if err := os.Remove(stable); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target, stable); err != nil {
			t.Fatal(err)
		}
		got, err := invokedBinary(stable, target)
		if err != nil || got != stable {
			t.Fatal("pinned versioned package path", got, err)
		}
		t.Setenv("PATH", filepath.Dir(stable))
		got, err = invokedBinary("zalki", target)
		if err != nil || got != stable {
			t.Fatal("PATH invocation lost stable path", got, err)
		}
	}
	if _, err := invokedBinary(stable, os.Args[0]); err == nil {
		t.Fatal("accepted another executable as the installation command")
	}
}
