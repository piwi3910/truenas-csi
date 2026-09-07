package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// smbUser creates a temporary SMB-capable account on the appliance and removes
// it afterwards. SMB, unlike NFS, authenticates, so an end-to-end check needs a
// real user rather than a host allowance.
func (e *env) smbUser(t *testing.T) (string, string) {
	t.Helper()
	name := "csi-e2e-" + uniqueName("u")[len("u")+1:]
	if len(name) > 16 {
		name = name[:16]
	}
	password := "Csi-E2E-" + uniqueName("p")
	var created any
	if err := e.client.CallJSON(context.Background(), &created, "user.create", map[string]any{
		"username": name, "full_name": "truenas-csi e2e", "password": password,
		"group_create": true, "smb": true,
	}); err != nil {
		t.Skipf("cannot create an SMB user on the appliance (needs ACCOUNT_WRITE): %v", err)
	}
	t.Cleanup(func() {
		var users []struct {
			ID int `json:"id"`
		}
		if err := e.client.CallJSON(context.Background(), &users, "user.query",
			[]any{[]any{"username", "=", name}}, map[string]any{}); err == nil && len(users) > 0 {
			_ = e.client.CallJSON(context.Background(), nil, "user.delete", users[0].ID, map[string]any{})
		}
	})
	return name, password
}

// TestE2ESMBProvision provisions an SMB volume through the real controller and
// mounts it from a cluster node with cifs.
func TestE2ESMBProvision(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)

	user, password := e.smbUser(t)
	vol := e.createVolume(t, c, uniqueName("pvc-e2e-smb"), "smb", 1<<30, nil)
	ctx := vol.GetVolumeContext()
	share := ctx["share"]
	if share == "" {
		t.Fatalf("the volume context carries no share name: %v", redactedKeys(ctx))
	}
	server := ctx["server"]
	if server == "" {
		server = e.server
	}
	mp := "/tmp/e2e-smb-" + uniqueName("m")

	// The password goes into a credentials file, never onto a command line where
	// it would sit in the node's process table.
	script := fmt.Sprintf(`set -e
umask 077
printf 'username=%[1]s\npassword=%[2]s\n' > /tmp/.csi-e2e-creds
mkdir -p %[3]s
mount -t cifs //%[4]s/%[5]s %[3]s -o credentials=/tmp/.csi-e2e-creds,vers=3.0
echo "smb payload" > %[3]s/s.txt
cat %[3]s/s.txt
echo "REPORTED_SIZE=$(df -k %[3]s | tail -1 | awk '{print $2}')"
umount %[3]s; rmdir %[3]s; rm -f /tmp/.csi-e2e-creds`, user, password, mp, server, share)

	out, err := r.Run(context.Background(), script)
	if err != nil {
		t.Fatalf("mounting the SMB volume failed: %v\n%s", err, redactOutput(out, password))
	}
	if !strings.Contains(out, "smb payload") {
		t.Fatalf("could not read back what was written:\n%s", redactOutput(out, password))
	}
	sizeKiB := extractInt(t, out, "REPORTED_SIZE=")
	if sizeKiB > 4*1048576 {
		t.Fatalf("the SMB share reports %d KiB for a 1 GiB volume — refquota is not applied "+
			"and the client can see the whole pool", sizeKiB)
	}
	t.Logf("SMB volume mounted and reported %d KiB for a 1 GiB request", sizeKiB)
}

// redactOutput keeps a credential out of a failure message.
func redactOutput(out, secret string) string {
	if secret == "" {
		return out
	}
	return strings.ReplaceAll(out, secret, "[redacted]")
}
