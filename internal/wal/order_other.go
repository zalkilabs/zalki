//go:build !darwin

package wal

import "os"

func orderBatchFile(file *os.File) error { return file.Sync() }
