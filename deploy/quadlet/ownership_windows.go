//go:build windows

package quadlet

import "os"

// Quadlet is unavailable on Windows; never adopt host files as Linux units.
func ownedByProcess(os.FileInfo) bool { return false }
