// +build !linux

package lumberjack

import (
	"os"
)

func chown(_ string, _ os.FileInfo) error {
	return nil
}

// chownExisting is a no-op anywhere but linux.
func chownExisting(_ string, _ os.FileInfo) error {
	return nil
}
