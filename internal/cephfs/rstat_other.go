//go:build !linux

package cephfs

import "runtime"

// GetRStat is only implemented on Linux: the virtual xattrs it reads are
// provided by the Linux CephFS kernel client. This stub keeps the package
// buildable elsewhere so the operator can be unit tested on a workstation.
func GetRStat(path string) RStat {
	return RStat{
		Path:          path,
		QuotaMaxBytes: NoQuota,
		Err:           "cephfs rstat probing requires linux (GOOS=" + runtime.GOOS + ")",
	}
}

// GetRStats reads the rstat of several directories.
func GetRStats(paths []string) []RStat {
	out := make([]RStat, 0, len(paths))
	for _, p := range paths {
		out = append(out, GetRStat(p))
	}
	return out
}

// IsCephFS cannot be determined off Linux.
func IsCephFS(string) (bool, error) { return false, nil }

// Statfs is only meaningful on Linux.
func Statfs(string) (Usage, error) { return Usage{}, nil }
