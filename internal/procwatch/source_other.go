//go:build !darwin && !linux

package procwatch

// NewSource returns ErrUnsupported on platforms without exit notifications.
func NewSource() (Source, error) { return nil, ErrUnsupported }
