//go:build linux

package cephfs

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

// Virtual xattrs exposed by the Linux CephFS client for a directory.
const (
	AttrRBytes        = "ceph.dir.rbytes"
	AttrRFiles        = "ceph.dir.rfiles"
	AttrRSubdirs      = "ceph.dir.rsubdirs"
	AttrQuotaMaxBytes = "ceph.quota.max_bytes"
)

// GetRStat reads the rstat virtual xattrs of a single CephFS directory.
//
// The read is read-only with respect to data and is safe to issue at any
// time; as a side effect it revalidates the calling kernel client's cached
// inode against the auth MDS.
func GetRStat(path string) RStat {
	st := RStat{Node: hostname(), Path: path, QuotaMaxBytes: NoQuota}

	rbytes, err := getxattrInt(path, AttrRBytes)
	if err != nil {
		st.Err = fmt.Sprintf("get %s: %v", AttrRBytes, err)
		return st
	}
	st.RBytes = rbytes

	if v, err := getxattrInt(path, AttrRFiles); err == nil {
		st.RFiles = v
	}
	if v, err := getxattrInt(path, AttrRSubdirs); err == nil {
		st.RSubdirs = v
	}
	if v, err := getxattrInt(path, AttrQuotaMaxBytes); err == nil {
		st.QuotaMaxBytes = v
	}
	return st
}

// GetRStats reads the rstat of several directories.
func GetRStats(paths []string) []RStat {
	out := make([]RStat, 0, len(paths))
	for _, p := range paths {
		out = append(out, GetRStat(p))
	}
	return out
}

// IsCephFS reports whether path lives on a CephFS superblock
// (CEPH_SUPER_MAGIC = 0x00c36400), so callers can skip non-Ceph paths instead
// of reporting a misleading zero.
func IsCephFS(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, err
	}
	return uint32(st.Type) == 0x00c36400, nil
}

// Statfs performs the statfs(2) call kubelet performs, on the very same path,
// so the agent can quantify the kernel cache staleness without depending on
// the metrics pipeline.
func Statfs(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bs := int64(st.Bsize)
	return Usage{
		Used:      (int64(st.Blocks) - int64(st.Bfree)) * bs,
		Capacity:  int64(st.Blocks) * bs,
		Available: int64(st.Bavail) * bs,
	}, nil
}

// getxattrInt reads an xattr whose payload is a decimal ASCII integer, which
// is how the kernel client renders the ceph.dir.* and ceph.quota.* values.
func getxattrInt(path, name string) (int64, error) {
	raw, err := getxattrString(path, name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q as integer: %w", raw, err)
	}
	return v, nil
}

func getxattrString(path, name string) (string, error) {
	buf := make([]byte, 128)
	n, err := syscall.Getxattr(path, name, buf)
	if err == syscall.ERANGE {
		buf = make([]byte, 4096)
		n, err = syscall.Getxattr(path, name, buf)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(buf[:n]), "\x00"), nil
}
