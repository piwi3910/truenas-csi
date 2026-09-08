//go:build probe

// Command bench measures per-protocol volume performance through the real
// driver: it provisions and publishes with the controller, mounts on a cluster
// node exactly as the node plugin would, and runs fio from a container against
// the host mount.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	_ "github.com/piwi3910/truenas-csi/internal/backend/iscsi"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nfs"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nvme"
	_ "github.com/piwi3910/truenas-csi/internal/backend/smb"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	node   = "worker-22"
	nsName = "default"
	size   = 8 << 30
)

type result struct {
	proto                    string
	randReadIOPS, randWrIOPS float64
	seqReadMBs, seqWriteMBs  float64
	readLatUs, writeLatUs    float64
	err                      string
}

func main() {
	b := config.Backend{
		Name: "nas1", Endpoint: os.Getenv("TN_EP"), Username: os.Getenv("TN_USER"),
		APIKey: os.Getenv("TN_KEY"), Pool: os.Getenv("TN_POOL"),
		ParentDataset: "csi-bench", Flavour: "scale", InsecureSkipVerify: true,
	}
	cfg := &config.Config{NodeID: node, Backends: map[string]config.Backend{"nas1": b}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	reg, err := backend.NewRegistry(ctx, cfg)
	if err != nil {
		fmt.Println("registry:", err)
		os.Exit(1)
	}
	defer reg.Close()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		fmt.Println("kubeconfig:", err)
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		fmt.Println("clientset:", err)
		os.Exit(1)
	}
	ctrl := csi.NewControllerWithNodes(reg, cfg, backend.NewKubeNodeResolverFor(cs))

	protos := strings.Split(os.Getenv("BENCH_PROTOCOLS"), ",")
	var results []result
	for _, p := range protos {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		fmt.Printf("\n=== %s ===\n", p)
		r := run(ctx, ctrl, p)
		results = append(results, r)
		if r.err != "" {
			fmt.Println("  FAILED:", r.err)
		}
	}

	fmt.Printf("\n\n%-8s %12s %12s %12s %12s %11s %11s\n",
		"PROTO", "randrd IOPS", "randwr IOPS", "seqrd MB/s", "seqwr MB/s", "rd lat us", "wr lat us")
	fmt.Println(strings.Repeat("-", 86))
	for _, r := range results {
		if r.err != "" {
			fmt.Printf("%-8s %s\n", r.proto, "FAILED: "+r.err)
			continue
		}
		fmt.Printf("%-8s %12.0f %12.0f %12.1f %12.1f %11.0f %11.0f\n",
			r.proto, r.randReadIOPS, r.randWrIOPS, r.seqReadMBs, r.seqWriteMBs, r.readLatUs, r.writeLatUs)
	}
}

func run(ctx context.Context, ctrl csipb.ControllerServer, proto string) result {
	res := result{proto: proto}
	name := fmt.Sprintf("pvc-bench-%s-%d", proto, time.Now().Unix())
	params := map[string]string{"backend": "nas1", "protocol": proto}
	if proto == "smb" {
		params["secretName"] = "bench-smb"
		params["secretNamespace"] = nsName
	}
	mode := csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	if proto == "nfs" || proto == "smb" {
		mode = csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	}
	vc := &csipb.VolumeCapability{
		AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
		AccessMode: &csipb.VolumeCapability_AccessMode{Mode: mode},
	}
	cv, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: name, Parameters: params, CapacityRange: &csipb.CapacityRange{RequiredBytes: size},
		VolumeCapabilities: []*csipb.VolumeCapability{vc},
	})
	if err != nil {
		res.err = "CreateVolume: " + err.Error()
		return res
	}
	id := cv.GetVolume().GetVolumeId()
	defer func() {
		dctx, dc := context.WithTimeout(context.Background(), 3*time.Minute)
		defer dc()
		_, _ = ctrl.ControllerUnpublishVolume(dctx, &csipb.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: node})
		if _, err := ctrl.DeleteVolume(dctx, &csipb.DeleteVolumeRequest{VolumeId: id}); err != nil {
			fmt.Println("  cleanup DeleteVolume:", err)
		}
	}()

	pub, err := ctrl.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: node, VolumeCapability: vc,
	})
	if err != nil {
		res.err = "ControllerPublishVolume: " + err.Error()
		return res
	}
	pc := map[string]string{}
	for k, v := range cv.GetVolume().GetVolumeContext() {
		pc[k] = v
	}
	for k, v := range pub.GetPublishContext() {
		pc[k] = v
	}

	mp := "/tmp/bench-" + proto
	mount, umount, err := scripts(proto, pc, mp)
	if err != nil {
		res.err = err.Error()
		return res
	}
	out, err := runPod(ctx, proto, mount, umount, mp)
	if err != nil {
		res.err = err.Error() + "\n" + tail(out, 25)
		return res
	}
	parse(&res, out)
	if res.randReadIOPS == 0 && res.seqReadMBs == 0 {
		res.err = "no fio results parsed\n" + tail(out, 30)
	}
	return res
}

func scripts(proto string, pc map[string]string, mp string) (string, string, error) {
	switch proto {
	case "nfs":
		v := pc["nfsVersion"]
		if v == "" {
			v = "4"
		}
		return fmt.Sprintf("mkdir -p %s\nmount -t nfs -o vers=%s %s:%s %s\n",
			mp, v, pc["server"], pc["share"], mp), "umount " + mp + "\n", nil
	case "smb":
		return fmt.Sprintf("mkdir -p %s\nprintf 'username=%s\\npassword=%s\\n' > /tmp/bench.cred\nchmod 600 /tmp/bench.cred\nmount -t cifs //%s/%s %s -o credentials=/tmp/bench.cred,vers=3.0\nrm -f /tmp/bench.cred\n",
				mp, os.Getenv("BENCH_SMB_USER"), os.Getenv("BENCH_SMB_PASS"), pc["server"], pc["share"], mp),
			"umount " + mp + "\n", nil
	case "iscsi":
		naa := strings.TrimPrefix(pc["naa"], "0x")
		auth := ""
		if pc["chapUser"] != "" {
			auth = fmt.Sprintf("iscsiadm -m node -T %s -p %s --op update -n node.session.auth.authmethod -v CHAP\niscsiadm -m node -T %s -p %s --op update -n node.session.auth.username -v '%s'\niscsiadm -m node -T %s -p %s --op update -n node.session.auth.password -v '%s'\n",
				pc["iqn"], pc["portal"], pc["iqn"], pc["portal"], pc["chapUser"], pc["iqn"], pc["portal"], pc["chapSecret"])
		}
		return fmt.Sprintf(`iscsiadm -m discovery -t sendtargets -p %[1]s >/dev/null
%[4]s
iscsiadm -m node -T %[2]s -p %[1]s --login >/dev/null
sleep 4
DEV=$(readlink -f /dev/disk/by-id/scsi-3%[3]s)
echo "DEVICE=$DEV"
mkfs.ext4 -F -q "$DEV"
mkdir -p %[5]s
mount "$DEV" %[5]s
`, pc["portal"], pc["iqn"], naa, auth, mp),
			fmt.Sprintf("umount %s\niscsiadm -m node -T %s -p %s --logout >/dev/null\niscsiadm -m node -o delete -T %s -p %s >/dev/null 2>&1 || true\n",
				mp, pc["iqn"], pc["portal"], pc["iqn"], pc["portal"]), nil
	case "nvme":
		// Device resolution keys on the SUBSYSTEM SERIAL via /dev/disk/by-id,
		// never on an index. internal/node/nvme.go carries the reason in
		// capitals: worker-21 has its own NVMe SSD at /dev/nvme0n1, so picking
		// an index can hand you the node's own disk -- and the next line is
		// mkfs. If the by-id link is absent this aborts rather than guessing.
		host, port, ok := strings.Cut(pc["portal"], ":")
		if !ok {
			host, port = pc["portal"], "4420"
		}
		return fmt.Sprintf(`nvme connect -t %[6]s -a %[1]s -s %[2]s -n %[3]s
sleep 4
DEV=""
for L in /dev/disk/by-id/nvme-*_%[4]s /dev/disk/by-id/nvme-*%[4]s; do
  [ -e "$L" ] || continue
  DEV=$(readlink -f "$L"); break
done
if [ -z "$DEV" ]; then
  echo "FATAL: no /dev/disk/by-id link for serial %[4]s; refusing to guess a device" >&2
  ls -l /dev/disk/by-id/ >&2
  exit 1
fi
echo "DEVICE=$DEV"
mkfs.ext4 -F -q "$DEV"
mkdir -p %[5]s
mount "$DEV" %[5]s
`, host, port, pc["nqn"], pc["serial"], mp, orDef(pc["transport"], "tcp")),
			fmt.Sprintf("umount %s\nnvme disconnect -n %s >/dev/null 2>&1 || true\n", mp, pc["nqn"]), nil
	}
	return "", "", fmt.Errorf("unknown protocol %q", proto)
}

func orDef(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// fioPlan is four 20s runs: random 4k read and write at depth 32 for IOPS and
// latency, then 1M sequential read and write for bandwidth. direct=1 so the
// page cache does not report the host's RAM speed as the appliance's.
const fioPlan = `
for J in "randread 4k 32" "randwrite 4k 32" "read 1M 8" "write 1M 8"; do
  set -- $J
  echo "===FIO $1==="
  fio --name=$1 --rw=$1 --bs=$2 --iodepth=$3 --ioengine=libaio --direct=1 \
      --filename=/host%[1]s/fio.dat --size=2G --runtime=20 --ramp_time=5 \
      --time_based --group_reporting --output-format=json 2>/dev/null
  echo "===ENDFIO==="
done
rm -f /host%[1]s/fio.dat
`

func runPod(ctx context.Context, proto, mount, umount, mp string) (string, error) {
	name := "bench-" + proto
	script := "set -e\napk add --no-cache fio >/dev/null 2>&1\n" +
		"chroot /host sh -c " + shq(mount) + "\n" +
		fmt.Sprintf(fioPlan, mp) +
		"chroot /host sh -c " + shq(umount) + "\n"
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": nsName},
		"spec": map[string]any{
			"nodeName": node, "restartPolicy": "Never", "hostNetwork": true, "hostPID": true,
			"tolerations": []any{map[string]any{"operator": "Exists"}},
			"volumes":     []any{map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}}},
			"containers": []any{map[string]any{
				"name": "r", "image": "alpine:3.20",
				"command":         []string{"sh", "-c", script},
				"securityContext": map[string]any{"privileged": true},
				"volumeMounts": []any{map[string]any{
					"name": "host", "mountPath": "/host", "mountPropagation": "Bidirectional"}},
			}},
		},
	}
	b, _ := json.Marshal(pod)
	_ = exec.Command("kubectl", "delete", "pod", name, "-n", nsName, "--ignore-not-found", "--force", "--grace-period=0").Run()
	ap := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	ap.Stdin = strings.NewReader(string(b))
	if out, err := ap.CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("kubectl apply: %v", err)
	}
	defer exec.Command("kubectl", "delete", "pod", name, "-n", nsName, "--ignore-not-found", "--force", "--grace-period=0").Run()

	deadline := time.Now().Add(25 * time.Minute)
	for time.Now().Before(deadline) {
		ph, _ := exec.Command("kubectl", "get", "pod", name, "-n", nsName, "-o", "jsonpath={.status.phase}").Output()
		switch strings.TrimSpace(string(ph)) {
		case "Succeeded":
			o, _ := exec.Command("kubectl", "logs", name, "-n", nsName).CombinedOutput()
			return string(o), nil
		case "Failed":
			o, _ := exec.Command("kubectl", "logs", name, "-n", nsName).CombinedOutput()
			return string(o), fmt.Errorf("pod failed")
		}
		time.Sleep(5 * time.Second)
	}
	o, _ := exec.Command("kubectl", "logs", name, "-n", nsName).CombinedOutput()
	return string(o), fmt.Errorf("timed out")
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func tail(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// fioOutput is the slice of fio's JSON report this benchmark reads.
type fioOutput struct {
	Jobs []struct {
		Read  fioSide `json:"read"`
		Write fioSide `json:"write"`
	} `json:"jobs"`
}

type fioSide struct {
	IOPS  float64 `json:"iops"`
	BW    float64 `json:"bw"` // KiB/s
	LatNs struct {
		Mean float64 `json:"mean"`
	} `json:"lat_ns"`
}

// parse walks the marker-delimited fio reports the pod emitted. Markers rather
// than whole-log parsing because the pod also prints apk and mount chatter, and
// a truncated report must be visibly absent rather than decode silently to zero.
func parse(r *result, out string) {
	rest := out
	for {
		i := strings.Index(rest, "===FIO ")
		if i < 0 {
			return
		}
		rest = rest[i+len("===FIO "):]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return
		}
		kind := strings.TrimSuffix(strings.TrimSpace(rest[:nl]), "===")
		rest = rest[nl+1:]
		end := strings.Index(rest, "===ENDFIO===")
		if end < 0 {
			return
		}
		body := rest[:end]
		rest = rest[end+len("===ENDFIO==="):]

		var fo fioOutput
		if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &fo); err != nil || len(fo.Jobs) == 0 {
			continue
		}
		j := fo.Jobs[0]
		side := j.Read
		if strings.Contains(kind, "write") {
			side = j.Write
		}
		switch kind {
		case "randread":
			r.randReadIOPS, r.readLatUs = side.IOPS, side.LatNs.Mean/1000
		case "randwrite":
			r.randWrIOPS, r.writeLatUs = side.IOPS, side.LatNs.Mean/1000
		case "read":
			r.seqReadMBs = side.BW / 1024
		case "write":
			r.seqWriteMBs = side.BW / 1024
		}
	}
}
