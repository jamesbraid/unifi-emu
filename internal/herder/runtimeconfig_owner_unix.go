//go:build unix

package herder

import (
	"io/fs"
	"os"
	"syscall"
)

// checkOwner requires the runtime file to belong to the invoking user or to
// root. Ownership is the other half of the permission check: a file the
// invoking account does not own could be replaced wholesale by whoever does.
func checkOwner(path string, info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // no ownership information on this filesystem
	}
	uid := uint32(os.Getuid())
	if stat.Uid != 0 && stat.Uid != uid {
		return failf(CodeInvalidRuntimeConfig, PhaseValidate,
			"runtime configuration %s is owned by uid %d, want root or uid %d", path, stat.Uid, uid)
	}
	return nil
}
