#!/usr/bin/env bash
#
# Run the upstream Kubernetes external-storage E2E suite against this driver.
#
# WHY A SCRIPT AND A DOWNLOADED BINARY, NOT A GO TEST:
# the suite lives in k8s.io/kubernetes/test/e2e, which is the one Go module
# nobody should take a dependency on — it pulls the whole of Kubernetes plus
# every cloud provider client into go.mod, and k/k publishes no usable
# semantic-versioned module for it (its own go.mod pins k8s.io/* at v0.0.0 and
# relies on replace directives). A thin second module would still drag that
# tree into the repo and into every `go mod download`. The released `e2e.test`
# binary is the same code, built and tested by the release it ships with, and
# costs this repository exactly one shell script. It is also what upstream
# documents, and what Dell's cert-csi shelled out to from its own `k8s-e2e`
# subcommand before cert-csi was archived.
#
# ALL DOWNLOADS ARE https. The appliance itself is never contacted by this
# script, but the repository rule stands everywhere: plaintext to a TrueNAS
# appliance revokes its API key permanently.
#
# Usage:
#   TRUENAS_E2E_KUBECONFIG=~/.kube/config \
#   TRUENAS_E2E_STORAGECLASS=truenas-nfs \
#   TRUENAS_E2E_PROTOCOL=nfs \
#   test/external/run.sh
#
# See test/external/README.md for the full environment reference.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() {
	echo "run.sh: $*" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Inputs
# ---------------------------------------------------------------------------

kubeconfig="${TRUENAS_E2E_KUBECONFIG:-}"
storageclass="${TRUENAS_E2E_STORAGECLASS:-}"
protocol="${TRUENAS_E2E_PROTOCOL:-}"
snapshotclass="${TRUENAS_E2E_SNAPSHOTCLASS:-}"

[ -n "$kubeconfig" ] || die "TRUENAS_E2E_KUBECONFIG is required (path to a kubeconfig for a live cluster)"
[ -n "$storageclass" ] || die "TRUENAS_E2E_STORAGECLASS is required (an existing StorageClass backed by this driver)"
[ -n "$protocol" ] || die "TRUENAS_E2E_PROTOCOL is required (nfs, iscsi, nvme or smb)"
[ -r "$kubeconfig" ] || die "kubeconfig $kubeconfig is not readable"

definition="$here/testdriver-$protocol.yaml"
[ -r "$definition" ] || die "no driver definition for protocol $protocol ($definition)"

workdir="${TRUENAS_E2E_WORKDIR:-${TMPDIR:-/tmp}/truenas-csi-e2e}"
bindir="${TRUENAS_E2E_BINDIR:-$workdir/bin}"
reportdir="${TRUENAS_E2E_REPORTDIR:-$workdir/report}"
mkdir -p "$workdir" "$bindir" "$reportdir"

# ---------------------------------------------------------------------------
# The e2e.test binary, matched to the cluster
# ---------------------------------------------------------------------------
#
# Version skew between e2e.test and the API server produces failures that look
# like driver defects, so the cluster is asked what it is rather than a version
# being pinned here and going stale.

version="${TRUENAS_E2E_VERSION:-}"
if [ -z "$version" ]; then
	command -v kubectl >/dev/null 2>&1 || die "kubectl is not on PATH; set TRUENAS_E2E_VERSION to pin the e2e.test release instead"
	version="$(KUBECONFIG="$kubeconfig" kubectl version -o json 2>/dev/null |
		sed -n 's/.*"gitVersion" *: *"\(v[0-9][^"]*\)".*/\1/p' | tail -n1)"
	[ -n "$version" ] || die "could not read the cluster's server version; set TRUENAS_E2E_VERSION"
fi

# A distribution's gitVersion is not a release on dl.k8s.io. k3s reports
# v1.34.4+k3s1 and rke2 v1.34.4+rke2r1; both 404. EKS and friends append a
# vendor pre-release instead (v1.34.4-eks-1-34-5), which also 404s. Upstream's
# own pre-releases -- alpha, beta, rc -- are real downloads and must survive.
#
# Without this the suite could not run against k3s at all, which is what this
# repository is developed on, so it had never run.
version="${version%%+*}"
case "$version" in
*-alpha.* | *-beta.* | *-rc.*) ;;
*-*) version="${version%%-*}" ;;
esac

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported host OS $(uname -s)" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported host architecture $(uname -m)" ;;
esac

e2etest="$bindir/e2e.test-$version-$os-$arch"
if [ ! -x "$e2etest" ]; then
	url="https://dl.k8s.io/$version/kubernetes-test-$os-$arch.tar.gz"
	echo "run.sh: fetching $url"
	tmp="$(mktemp -d "$workdir/dl.XXXXXX")"
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL --proto '=https' --tlsv1.2 "$url" -o "$tmp/kubernetes-test.tar.gz" ||
		die "downloading $url failed"
	tar -xzf "$tmp/kubernetes-test.tar.gz" -C "$tmp" kubernetes/test/bin/e2e.test ||
		die "the release tarball does not contain kubernetes/test/bin/e2e.test"
	mv "$tmp/kubernetes/test/bin/e2e.test" "$e2etest"
	chmod +x "$e2etest"
	rm -rf "$tmp"
	trap - EXIT
fi

# ---------------------------------------------------------------------------
# Render the driver definition
# ---------------------------------------------------------------------------
#
# The checked-in definitions carry placeholders so that the capability flags,
# which are the reviewable part, live in git while the cluster-specific class
# names do not.

rendered="$workdir/testdriver-$protocol.rendered.yaml"

# The node plugin publishes one topology key per configured backend, named after
# it, so the definitions cannot carry it. It comes from the StorageClass under
# test, which already names the backend it provisions on. A class that names
# none (the single-backend case, where the parameter is optional) leaves the key
# out of the list entirely rather than rendering a wrong one.
backendkey=""
if [ -z "${TRUENAS_E2E_BACKEND:-}" ] && command -v kubectl >/dev/null 2>&1; then
	TRUENAS_E2E_BACKEND="$(KUBECONFIG="$kubeconfig" kubectl get storageclass "$storageclass" \
		-o jsonpath='{.parameters.backend}' 2>/dev/null || true)"
fi
if [ -n "${TRUENAS_E2E_BACKEND:-}" ]; then
	backendkey="csi.truenas.watteel.com/backend-$TRUENAS_E2E_BACKEND"
else
	echo "run.sh: StorageClass $storageclass names no backend and TRUENAS_E2E_BACKEND is unset --" >&2
	echo "run.sh: the per-backend topology key is omitted from the driver definition" >&2
fi

# No VolumeSnapshotClass named means the cluster may not even have the snapshot
# CRDs installed. Dropping the SnapshotClass block while leaving
# snapshotDataSource true would make every snapshot test fail on a missing class
# rather than on the driver, so the capability is dropped WITH it and the
# omission is announced. That is a statement about the cluster, not about the
# driver: the driver does implement CREATE_DELETE_SNAPSHOT.
if [ -z "$snapshotclass" ]; then
	echo "run.sh: TRUENAS_E2E_SNAPSHOTCLASS is unset — snapshot and restore tests will NOT run" >&2
fi

# One awk pass rather than a chain of `sed -i`: in-place editing and `addr,+N`
# ranges are spelt differently by GNU and BSD sed, and this script runs on both.
awk -v sc="$storageclass" -v vsc="$snapshotclass" -v bk="$backendkey" '
	# Drop the SnapshotClass mapping and its single child when no class is named.
	skipnext { skipnext = 0; next }
	vsc == "" && /^SnapshotClass:/ { skipnext = 1; next }
	vsc == "" && /^[[:space:]]*snapshotDataSource:[[:space:]]*true/ {
		sub(/true/, "false"); print; next
	}
	# The per-backend topology key is dropped entirely when there is none to
	# render: a literal placeholder would be checked for below, and an empty
	# list entry would be read as a key named "".
	bk == "" && /__BACKEND_TOPOLOGY_KEY__/ { next }
	{ gsub(/__STORAGE_CLASS__/, sc); gsub(/__SNAPSHOT_CLASS__/, vsc)
	  gsub(/__BACKEND_TOPOLOGY_KEY__/, bk); print }
' "$definition" >"$rendered"

if grep -q '__STORAGE_CLASS__\|__SNAPSHOT_CLASS__\|__BACKEND_TOPOLOGY_KEY__' "$rendered"; then
	die "the rendered definition still contains a placeholder: $rendered"
fi

# ---------------------------------------------------------------------------
# The skip list
# ---------------------------------------------------------------------------
#
# EVERY ENTRY CARRIES A REASON, AND "it fails" IS NOT ONE. An entry is either a
# capability this driver genuinely does not have — in which case it says which,
# and the capability flags in the definition already do most of that work — or
# an environmental cost, in which case it is opt-in-able rather than permanent.
#
# Note what is NOT here: nothing is skipped for volumeMode Block on NFS, for
# RWX on iSCSI, for fsGroup, or for offline expansion. Those are all expressed
# as capability flags in the driver definition, which is the honest place for
# them, and the framework then never generates the test at all.

skips=()

# The suite reboots nodes, kills kubelet and severs the network to prove the CO
# recovers. That needs a throwaway cluster and SSH to every node, neither of
# which this harness assumes.
#
# It is also where the appliance-side fence would be exercised. The driver HAS
# one now -- ControllerUnpublishVolume revokes access per node and the CSIDriver
# sets attachRequired: true -- so this entry is an environmental cost and no
# longer a missing capability. Running it needs a cluster the suite may break.
skips+=('\[Disruptive\]')

# Guarded by alpha or beta feature gates that must be enabled on the API server
# and kubelet. Whether they are on is a property of the cluster, not the driver;
# enable them and drop this entry to widen coverage deliberately.
skips+=('\[Feature:.*\]')
skips+=('\[FeatureGate:.*\]')

# Upstream marks these as failing intermittently in its own CI. A known-flaky
# test cannot distinguish a driver regression from noise, which is the only
# thing this harness is for.
skips+=('\[Flaky\]')

# Heavy by construction rather than unsupported. Both are ON by default because
# a routine run should finish; set TRUENAS_E2E_INCLUDE_HEAVY=true for a release
# candidate and expect hours.
if [ "${TRUENAS_E2E_INCLUDE_HEAVY:-}" != "true" ]; then
	# Upstream's own marker for tests measured in tens of minutes.
	skips+=('\[Slow\]')
	# volumeLimits: true is truthful — NodeGetInfo advertises
	# MaxVolumesPerNode = 128 — and the suite takes it literally, provisioning
	# 128 zvols or datasets per node on a live appliance to find the ceiling.
	skips+=('volume limits')
fi

if [ -n "${TRUENAS_E2E_EXTRA_SKIP:-}" ]; then
	# An operator's own additions, for triaging one failure without editing the
	# reviewed list above.
	skips+=("$TRUENAS_E2E_EXTRA_SKIP")
fi

skip="$(
	IFS='|'
	echo "${skips[*]}"
)"

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

# TRUENAS_E2E_FOCUS NARROWS the run; it does not redirect it.
#
# It used to replace the focus outright, so `TRUENAS_E2E_FOCUS=ephemeral` ran
# the upstream suite's ephemeral tests for ITS OWN in-tree drivers -- spinning
# up nfs-provisioner pods, failing on image pulls, and reporting failures that
# say nothing about this driver. A harness that cannot tell a driver regression
# from noise is the one thing this directory is not for.
#
# Repeated --ginkgo.focus arguments are ORed, not ANDed, so narrowing has to be
# one regular expression. A spec's text begins with the driver
# ("External Storage [Driver: csi.truenas.watteel.com] ..."), so anchoring the
# narrowing after it keeps the run inside this driver.
focus="External.Storage"
if [ -n "${TRUENAS_E2E_FOCUS:-}" ]; then
	focus="External.Storage.*${TRUENAS_E2E_FOCUS}"
fi
timeout="${TRUENAS_E2E_TIMEOUT:-4h}"

echo "run.sh: driver definition $rendered"
echo "run.sh: focus  $focus"
echo "run.sh: skip   $skip"
echo "run.sh: report $reportdir"

# --ginkgo.procs is deliberately absent: the suite is run serially. The driver
# serialises per-volume work with a lock and the appliance has a measured
# 20-call concurrency ceiling, so parallel specs would report contention as
# driver failures.
exec "$e2etest" \
	-context="${TRUENAS_E2E_CONTEXT:-}" \
	-kubeconfig="$kubeconfig" \
	-provider=local \
	-storage.testdriver="$rendered" \
	-report-dir="$reportdir" \
	--ginkgo.focus="$focus" \
	--ginkgo.skip="$skip" \
	--ginkgo.timeout="$timeout" \
	--ginkgo.junit-report="$reportdir/junit-$protocol.xml" \
	"$@"
