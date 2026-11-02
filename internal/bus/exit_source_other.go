//go:build !darwin && !linux

package bus

func newExitSource() (exitSource, error) { return nil, errExitSourceUnsupported }
