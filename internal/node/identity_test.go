package node

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInitiatorIQNSkipsTheCommentBlock pins the parsing of a file that is not
// what it looks like.
//
// /etc/iscsi/initiatorname.iscsi carries a three-line comment above the value,
// so reading the whole file -- which is correct for /etc/nvme/hostnqn -- yields
// the comments glued to the IQN. A first attempt at reading this during
// investigation produced exactly that:
//
//	"## If you change the InitiatorName...iqn.2004-10.com.ubuntu:01:5559e45717f4"
//
// An IQN like that would be written into a Node annotation and then into an
// appliance-side initiator group, where it matches nothing.
func TestInitiatorIQNSkipsTheCommentBlock(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "iscsi"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `## DO NOT EDIT OR REMOVE THIS FILE!
## If you change the InitiatorName, existing access control lists
## may reject this initiator.  The InitiatorName must be unique
## for each iSCSI initiator.  Do NOT duplicate iSCSI InitiatorNames.
InitiatorName=iqn.2004-10.com.ubuntu:01:5559e45717f4
`
	if err := os.WriteFile(filepath.Join(root, "etc", "iscsi", "initiatorname.iscsi"),
		[]byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := InitiatorIQN(root), "iqn.2004-10.com.ubuntu:01:5559e45717f4"; got != want {
		t.Errorf("InitiatorIQN = %q, want %q", got, want)
	}
}

// TestHostNQNReadsTheWholeLine covers the simpler file, and the absence of both.
func TestHostNQNReadsTheWholeLine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "nvme"), 0o755); err != nil {
		t.Fatal(err)
	}
	nqn := "nqn.2014-08.org.nvmexpress:uuid:2b216171-4c1f-4c8c-bfdf-dd3907bc2cff"
	if err := os.WriteFile(filepath.Join(root, "etc", "nvme", "hostnqn"),
		[]byte(nqn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HostNQN(root); got != nqn {
		t.Errorf("HostNQN = %q, want %q", got, nqn)
	}

	// A node without the tooling has neither file, and that is not an error:
	// it simply has no identity to publish.
	empty := t.TempDir()
	if got := HostNQN(empty); got != "" {
		t.Errorf("HostNQN on a node with no nvme-cli = %q, want empty", got)
	}
	if got := InitiatorIQN(empty); got != "" {
		t.Errorf("InitiatorIQN on a node with no open-iscsi = %q, want empty", got)
	}
}
