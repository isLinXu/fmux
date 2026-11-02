//go:build !darwin

package main

import "errors"

// rssMB is only implemented on macOS. Reading another process's resident size
// portably needs /proc, which does not exist here; the gateway itself does not
// run on other platforms either, so this only keeps the package buildable.
func rssMB(int) (int, error) {
	return 0, errors.New("resident size is not available on this platform")
}
