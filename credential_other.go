//go:build !linux

package tailrelay

import "os"

func isSystemdCredential(_ *os.File, _ string, _ os.FileInfo) bool { return false }
