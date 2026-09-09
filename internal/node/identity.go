// Identity discovery: what this node calls itself to a storage target.
package node

import (
	"os"
	"path/filepath"
	"strings"
)

// hostNQNPath and initiatorNamePath are where the initiators keep their own
// identity. Both are read relative to the node plugin's host root.
const (
	hostNQNPath       = "etc/nvme/hostnqn"
	initiatorNamePath = "etc/iscsi/initiatorname.iscsi"
)

// HostNQN reads this node's NVMe host NQN, or "" when the file is absent or
// empty.
//
// The file holds the NQN on one line and nothing else. An unreadable file is
// not an error to report: a node without nvme-cli installed legitimately has
// none, and the caller's job is to publish what it found rather than to insist.
func HostNQN(root string) string {
	b, err := os.ReadFile(filepath.Join(root, hostNQNPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// InitiatorIQN reads this node's iSCSI initiator name, or "" when it has none.
//
// Unlike the NVMe file, this one is a shell-style config with comments, so the
// value has to be picked out of it:
//
//	## If you change the InitiatorName, existing access control lists
//	InitiatorName=iqn.2004-10.com.ubuntu:01:5559e45717f4
//
// Taking the whole file, as one might for hostnqn, yields the comment block
// glued to the value -- which is what a first attempt at this actually produced.
func InitiatorIQN(root string) string {
	b, err := os.ReadFile(filepath.Join(root, initiatorNamePath))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "InitiatorName="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
