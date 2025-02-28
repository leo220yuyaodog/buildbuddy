//go:build linux && arm64

package vfs

import (
	"syscall"

	vfspb "github.com/buildbuddy-io/buildbuddy/proto/vfs"
)

func attrsToStat(attr *vfspb.Attrs) *syscall.Stat_t {
	return &syscall.Stat_t{
		Size:  attr.GetSize(),
		Mode:  attr.GetPerm(),
		Nlink: attr.GetNlink(),
	}
}
