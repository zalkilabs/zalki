//go:build !darwin && !linux

package platform

import "errors"

// File locks are not supported on this operating system.

func OpenLockFile(string, bool) (int, error) { return -1, errors.ErrUnsupported }
func LockExclusive(int, bool) error          { return errors.ErrUnsupported }
func LockShared(int, bool) error             { return errors.ErrUnsupported }
func LockBusy(error) bool                    { return false }
func Unlock(int) error                       { return errors.ErrUnsupported }
func CloseLockFile(int) error                { return errors.ErrUnsupported }
func Linked(int, string) (bool, error)       { return false, errors.ErrUnsupported }
