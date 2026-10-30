package tailrelay

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// systemd can expose LoadCredential files as root:root 0440, with a named-user
// ACL and a read-only mount private to the service. Do not apply this exception
// to state files, ordinary group-readable secrets or a caller-supplied directory.
func isSystemdCredential(file *os.File, path string, info os.FileInfo) bool {
	directory := os.Getenv("CREDENTIALS_DIRECTORY")
	if !filepath.IsAbs(directory) || directory != filepath.Clean(directory) ||
		!strings.HasPrefix(directory, "/run/credentials/") ||
		filepath.Dir(filepath.Clean(path)) != directory {
		return false
	}
	if info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o440 {
		return false
	}
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o222 != 0 {
		return false
	}
	var stat unix.Stat_t
	var fs unix.Statfs_t
	return unix.Fstat(int(file.Fd()), &stat) == nil && stat.Uid == 0 && stat.Gid == 0 &&
		unix.Fstatfs(int(file.Fd()), &fs) == nil && fs.Flags&unix.ST_RDONLY != 0
}
