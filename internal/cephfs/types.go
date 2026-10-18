// Package cephfs reads the directory accounting (rstat) that the Ceph MDS
// keeps for a directory, exposed by the Linux CephFS kernel client as virtual
// xattrs.
//
// Why this package exists rather than calling statfs(2) alone:
//
//	statfs(2) on a CephFS mount whose root carries a quota returns the
//	*client-side cached* inode fields (fs/ceph/quota.c::
//	ceph_quota_update_statfs() reads ci->i_rbytes directly). There is no MDS
//	round trip on that path, so the number can stay stale for months.
//	Reading the ceph.dir.rbytes vxattr goes through __ceph_getxattr(), which
//	routes CEPH_STAT_RSTAT to the authoritative MDS and writes the answer back
//	into ci->i_rbytes. That has two useful consequences:
//
//	 1. it is the only trustworthy source of "how much is in this subvolume";
//	 2. issuing the read on the node that owns the stale mount heals it, and
//	    the very next statfs(2) by kubelet returns the correct value.
package cephfs

import (
	"fmt"
	"os"
)

// NoQuota is what the client returns for ceph.quota.max_bytes when the
// directory has no quota of its own.
const NoQuota = -1

// RStat is the MDS-authoritative accounting for one directory.
type RStat struct {
	Node          string `json:"node,omitempty"`
	Path          string `json:"path"`
	RBytes        int64  `json:"rbytes"`
	RFiles        int64  `json:"rfiles"`
	RSubdirs      int64  `json:"rsubdirs"`
	QuotaMaxBytes int64  `json:"quota_max_bytes"`
	Err           string `json:"error,omitempty"`
}

// Available reports whether the directory responded to every rstat query.
func (r RStat) Available() bool { return r.Err == "" }

// String renders one line in roughly the shape of
// `getfattr --only-values -n ceph.dir.rbytes <dir>` output, plus context.
func (r RStat) String() string {
	if !r.Available() {
		return fmt.Sprintf("%-72s ERROR %s", r.Path, r.Err)
	}
	return fmt.Sprintf("%-72s rbytes=%-14d rfiles=%-8d quota=%-14d",
		r.Path, r.RBytes, r.RFiles, r.QuotaMaxBytes)
}

// Usage is what statfs(2) reports, i.e. the same numbers kubelet turns into
// kubelet_volume_stats_{used,capacity,available}_bytes.
type Usage struct {
	Used      int64 `json:"usedBytes"`
	Capacity  int64 `json:"capacityBytes"`
	Available int64 `json:"availableBytes"`
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
