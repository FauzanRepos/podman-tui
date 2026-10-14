//go:build !windows

package quadlet

import (
	"os"
	"syscall"
)

func ownedByProcess(fi os.FileInfo) bool {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
