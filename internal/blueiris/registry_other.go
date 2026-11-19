//go:build !windows

package blueiris

import "errors"

// ReadWindowsRegistry is only supported on Windows; pass an exported .reg file on other platforms.
func ReadWindowsRegistry() ([]RawCamera, error) {
	return nil, errors.New("direct Blue Iris registry import is only supported on Windows; pass a .reg file path instead")
}
