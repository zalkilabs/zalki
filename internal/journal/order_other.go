//go:build !darwin

package journal

import "os"

func orderBatchFile(file *os.File) error { return file.Sync() }
