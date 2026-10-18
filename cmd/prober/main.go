// Command prober reads the MDS-authoritative directory accounting
// (ceph.dir.rbytes / ceph.dir.rfiles / ceph.quota.max_bytes) for one or more
// CephFS mount paths.
//
// It is the primitive behind both halves of statfs-guard:
//
//	# ground truth  — the real number of bytes inside a subvolume
//	statfs-guard-prober --format json /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/pvc-xxx/mount
//	{"node":"node-1","path":"...","rbytes":10737418240,"rfiles":12345,"quota_max_bytes":107374182400}
//
//	# repair        — issuing the read forces this kernel client to revalidate
//	#                 the cached inode against the auth MDS, after which
//	#                 kubelet's statfs()-derived usage is correct again
//
// Exit status: 0 if at least one path was read successfully, 1 otherwise.
// A per-path failure is reported in the "error" field, not by crashing, so a
// single broken mount cannot hide the rest of the fleet.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/zzhkhyl/statfs-guard-operator/internal/cephfs"
)

func main() {
	format := flag.String("format", "json", "output format: json | text")
	quiet := flag.Bool("quiet", false, "suppress human-readable summary on stderr")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [-format json|text] <cephfs-path> [path...]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	stats := cephfs.GetRStats(paths)

	ok := 0
	for _, s := range stats {
		if s.Available() {
			ok++
		}
	}

	switch strings.ToLower(*format) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if len(stats) == 1 {
			_ = enc.Encode(stats[0])
		} else {
			_ = enc.Encode(stats)
		}
	case "text":
		for _, s := range stats {
			fmt.Println(s.String())
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown -format %q\n", *format)
		os.Exit(1)
	}

	if !*quiet {
		fmt.Fprintf(os.Stderr, "statfs-guard-prober: %d/%d path(s) read\n", ok, len(stats))
	}
	if ok == 0 {
		os.Exit(1)
	}
}
