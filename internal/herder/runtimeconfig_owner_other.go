//go:build !unix

package herder

import "io/fs"

// checkOwner is a no-op where the filesystem exposes no POSIX ownership. The
// herder targets Linux runners; this exists so the package still builds.
func checkOwner(string, fs.FileInfo) error { return nil }
