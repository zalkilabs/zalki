//go:build !darwin && !linux

package platform

import "errors"

// InspectSocket is not supported on this operating system.
func InspectSocket(string) (SocketID, error) {
	return SocketID{}, errors.ErrUnsupported
}
