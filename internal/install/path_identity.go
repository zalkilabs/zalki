package install

import (
	"errors"
	"os"
	"path/filepath"
)

func sameTargetFile(a, b string) (bool, error) {
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
	leftPath, err := resolvedTargetPath(a)
	if err != nil {
		return false, err
	}
	rightPath, err := resolvedTargetPath(b)
	return leftPath == rightPath, err
}

// Resolve the existing parent of a future target, retaining its missing suffix.
func resolvedTargetPath(path string) (string, error) {
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
