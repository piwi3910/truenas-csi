package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// NodeRunner runs a shell command in a real cluster node's host namespace.
//
// The node half of a CSI driver mounts filesystems, so verifying it needs a
// Linux host that can actually mount. Asserting against a mock would let a
// broken mount path pass, so these tests run their commands on a real node.
type NodeRunner struct {
	Node      string
	Namespace string
	Timeout   time.Duration
}

// NewNodeRunner returns a runner, or nil plus the reason to skip.
func NewNodeRunner() (*NodeRunner, string) {
	node := os.Getenv("TRUENAS_E2E_NODE")
	if node == "" {
		return nil, "TRUENAS_E2E_NODE is not set: skipping the node-side end-to-end checks. " +
			"Set it to a Linux cluster node name (with kubectl configured) to run them."
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil, "kubectl is not on PATH: skipping the node-side end-to-end checks"
	}
	ns := os.Getenv("TRUENAS_E2E_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return &NodeRunner{Node: node, Namespace: ns, Timeout: 6 * time.Minute}, ""
}

// podManifest builds a one-shot privileged pod pinned to the node, which runs
// the script in the host's namespaces. A explicit manifest is used rather than
// `kubectl debug` so the pod's name is known and its logs can be collected.
func (r *NodeRunner) podManifest(name, script string) string {
	spec := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": r.Namespace,
			"labels": map[string]string{"app": "truenas-csi-e2e"}},
		"spec": map[string]any{
			"nodeName": r.Node, "restartPolicy": "Never",
			"hostPID": true, "hostNetwork": true,
			"tolerations": []any{map[string]any{"operator": "Exists"}},
			"volumes": []any{map[string]any{
				"name": "host", "hostPath": map[string]any{"path": "/"}}},
			"containers": []any{map[string]any{
				"name": "runner", "image": "busybox:1.36",
				"command":         []string{"chroot", "/host", "sh", "-c", script},
				"securityContext": map[string]any{"privileged": true},
				"volumeMounts": []any{map[string]any{
					"name": "host", "mountPath": "/host",
					// Bidirectional, not the default None: this pod chroots into
					// /host and runs the host's mount binaries, so a mount it
					// makes must land in the HOST's namespace. With the default
					// the mount is private to this pod and disappears when the
					// pod exits -- so a later runner, and the host itself, see
					// nothing. A test that mounts and verifies inside one script
					// never notices; one that stages in one call and asserts in
					// the next sees the driver "succeed" against an empty mount
					// table. That is the same failure that once made every
					// volume look remote while it was silently node-local.
					"mountPropagation": "Bidirectional"}},
			}},
		},
	}
	b, _ := json.Marshal(spec)
	return string(b)
}

// Run executes a script on the node and returns its combined output.
func (r *NodeRunner) Run(ctx context.Context, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	name := fmt.Sprintf("csi-e2e-%d", time.Now().UnixNano()%1_000_000_000)
	apply := exec.CommandContext(ctx, "kubectl", "apply", "-n", r.Namespace, "-f", "-")
	apply.Stdin = strings.NewReader(r.podManifest(name, script))
	var applyOut bytes.Buffer
	apply.Stdout, apply.Stderr = &applyOut, &applyOut
	if err := apply.Run(); err != nil {
		return applyOut.String(), fmt.Errorf("creating the runner pod: %w: %s", err, applyOut.String())
	}
	defer func() {
		_ = exec.Command("kubectl", "delete", "pod", "-n", r.Namespace, name,
			"--wait=false", "--ignore-not-found").Run()
	}()

	var lastPhase string
	for {
		phase, _ := exec.CommandContext(ctx, "kubectl", "get", "pod", "-n", r.Namespace, name,
			"-o", "jsonpath={.status.phase}").Output()
		lastPhase = strings.TrimSpace(string(phase))
		if lastPhase == "Succeeded" || lastPhase == "Failed" {
			break
		}
		select {
		case <-ctx.Done():
			logs, _ := exec.Command("kubectl", "logs", "-n", r.Namespace, name).CombinedOutput()
			return string(logs), fmt.Errorf("runner pod did not finish (phase %q): %w", lastPhase, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}

	logs, err := exec.CommandContext(ctx, "kubectl", "logs", "-n", r.Namespace, name).CombinedOutput()
	if err != nil {
		return string(logs), fmt.Errorf("reading runner pod logs: %w", err)
	}
	if lastPhase == "Failed" {
		return string(logs), fmt.Errorf("the node script exited non-zero:\n%s", logs)
	}
	return string(logs), nil
}
