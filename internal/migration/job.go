package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mount points and volume names inside the copy Job. They are exported because
// the read-only-ness of the source mount is a safety property tests assert on.
const (
	SourceVolumeName = "source"
	TargetVolumeName = "target"
	SourceMountPath  = "/source"
	TargetMountPath  = "/target"
	workVolumeName   = "work"
	workMountPath    = "/work"
	containerName    = "copy"
)

// DefaultImage is the image the copy Job runs. It needs a POSIX shell, find,
// sha256sum and ideally rsync; the script falls back to tar when rsync is
// absent, so a minimal image still works.
const DefaultImage = "docker.io/library/alpine:3.22"

// JobNamePrefix begins every generated Job name, so an operator can find and
// clean up migrations without knowing which volumes were involved.
const JobNamePrefix = "truenas-csi-migrate-"

// Mode selects the copy mechanism.
type Mode string

const (
	// ModeAuto uses rsync when the image has it and tar otherwise. It is the
	// default because it is the only mode that works on an unknown image.
	ModeAuto Mode = "auto"
	// ModeRsync requires rsync and fails the Job if it is absent.
	ModeRsync Mode = "rsync"
	// ModeTar always uses tar. Whole-file only: a resumed tar copy re-reads
	// everything, where rsync skips what already matches.
	ModeTar Mode = "tar"
)

// defaultBackoffLimit lets a failed copy retry. Retrying is safe because the
// copy is resumable: rsync re-runs over a partial target and transfers only
// what differs.
const defaultBackoffLimit int32 = 4

// jobName derives a deterministic, DNS-1123 name from the two claims, so that
// re-running a migration adopts the existing Job instead of starting a second
// copy into the same volume.
func jobName(namespace, sourcePVC, targetPVC string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + sourcePVC + "\x00" + targetPVC))
	return JobNamePrefix + hex.EncodeToString(sum[:8])
}

// copyScript renders the shell program the Job runs.
//
// Every safety rule of a migration lives in this script, so it is written to be
// read: the source is only ever an rsync/tar source, nothing removes anything,
// and the script exits non-zero unless the two checksum manifests match.
func copyScript(mode Mode) string {
	var copyStep string
	rsync := fmt.Sprintf(`rsync -aHAX --numeric-ids --partial --inplace --stats %q/ %q/`,
		SourceMountPath, TargetMountPath)
	// tar preserves ownership and permissions too, but has no notion of a
	// partial transfer: it rewrites every file on a resumed run.
	tarPipe := fmt.Sprintf(`tar -C %q -cf - . | tar -C %q -xpf -`, SourceMountPath, TargetMountPath)

	switch mode {
	case ModeRsync:
		copyStep = `if ! command -v rsync >/dev/null 2>&1; then
  echo "mode=rsync but rsync is not present in this image" >&2
  exit 1
fi
` + rsync
	case ModeTar:
		copyStep = tarPipe
	default:
		copyStep = `if command -v rsync >/dev/null 2>&1; then
  echo "copying with rsync (resumable)"
  ` + rsync + `
else
  echo "rsync is absent; copying with tar (whole-file)"
  ` + tarPipe + `
fi`
	}

	return strings.Join([]string{
		`set -eu`,
		``,
		`# The source is mounted read-only by the pod spec. Nothing in this script`,
		`# writes to it, and nothing in this script deletes anything anywhere: the`,
		`# source volume is retired by the operator, never by the migration.`,
		fmt.Sprintf(`test -d %q || { echo "source is not mounted" >&2; exit 1; }`, SourceMountPath),
		fmt.Sprintf(`test -d %q || { echo "target is not mounted" >&2; exit 1; }`, TargetMountPath),
		fmt.Sprintf(`test -w %q || { echo "target is not writable" >&2; exit 1; }`, TargetMountPath),
		``,
		copyStep,
		``,
		`# Verification: a checksum manifest of every regular file on each side.`,
		`# The manifests are built under ` + workMountPath + `, never inside either volume,`,
		`# so the target's own contents are exactly what was copied.`,
		`manifest() {`,
		`  ( cd "$1" && find . -type f -print | LC_ALL=C sort | sed "s|^\./||" | \`,
		`      while IFS= read -r f; do sha256sum "$f"; done )`,
		`}`,
		fmt.Sprintf(`manifest %q > %s/source.manifest`, SourceMountPath, workMountPath),
		fmt.Sprintf(`manifest %q > %s/target.manifest`, TargetMountPath, workMountPath),
		``,
		fmt.Sprintf(`SRC_N=$(wc -l < %s/source.manifest | tr -d " ")`, workMountPath),
		fmt.Sprintf(`DST_N=$(wc -l < %s/target.manifest | tr -d " ")`, workMountPath),
		fmt.Sprintf(`SRC_D=$(sha256sum < %s/source.manifest | cut -d" " -f1)`, workMountPath),
		fmt.Sprintf(`DST_D=$(sha256sum < %s/target.manifest | cut -d" " -f1)`, workMountPath),
		fmt.Sprintf(`BYTES=$(du -sk %q | cut -f1)`, TargetMountPath),
		`BYTES=$((BYTES * 1024))`,
		``,
		`VERIFIED=false`,
		`if [ "$SRC_N" = "$DST_N" ] && [ "$SRC_D" = "$DST_D" ]; then VERIFIED=true; fi`,
		``,
		`printf '{"source_files":%s,"target_files":%s,"source_digest":"%s","target_digest":"%s","bytes_copied":%s,"verified":%s}' \`,
		`  "$SRC_N" "$DST_N" "$SRC_D" "$DST_D" "$BYTES" "$VERIFIED" > /dev/termination-log`,
		``,
		`if [ "$VERIFIED" != true ]; then`,
		`  echo "verification failed: $SRC_N/$DST_N files, $SRC_D vs $DST_D" >&2`,
		fmt.Sprintf(`  diff %s/source.manifest %s/target.manifest | head -n 50 >&2 || true`,
			workMountPath, workMountPath),
		`  exit 1`,
		`fi`,
		`echo "verified: $SRC_N files, manifest $SRC_D"`,
		``,
	}, "\n")
}

// buildJob renders the copy Job. The source PVC is mounted read-only in both
// the volume and the mount, which is what makes it impossible for a migration
// to modify what it is copying from.
func buildJob(p *Plan) *batchv1.Job {
	backoff := p.BackoffLimit
	if backoff == 0 {
		backoff = defaultBackoffLimit
	}
	yes, no := true, false
	root := int64(0)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.JobName,
			Namespace: p.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "truenas-csi-migration",
				"app.kubernetes.io/component": "copy",
				"truenas-csi.io/source-pvc":   p.Source.Claim,
				"truenas-csi.io/target-pvc":   p.Target.Claim,
			},
			Annotations: map[string]string{
				"truenas-csi.io/source-driver":  p.Source.Driver,
				"truenas-csi.io/target-handle":  p.Target.Handle,
				"truenas-csi.io/target-dataset": p.TargetDataset,
				"truenas-csi.io/mode":           string(p.Mode),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/name": "truenas-csi-migration"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: p.ServiceAccount,
					SecurityContext: &corev1.PodSecurityContext{
						// The copy runs as root so that -aHAX can restore the
						// source's uids, gids, ACLs and xattrs. It is confined
						// by dropping every capability it does not need.
						RunAsUser:  &root,
						RunAsGroup: &root,
					},
					Volumes: []corev1.Volume{
						{
							Name: SourceVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.Source.Claim,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: TargetVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.Target.Claim,
									ReadOnly:  false,
								},
							},
						},
						{
							Name:         workVolumeName,
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
					Containers: []corev1.Container{{
						Name:    containerName,
						Image:   p.Image,
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{copyScript(p.Mode)},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &no,
							Privileged:               &no,
							RunAsNonRoot:             &no,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add: []corev1.Capability{
									"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID",
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: SourceVolumeName, MountPath: SourceMountPath, ReadOnly: yes},
							{Name: TargetVolumeName, MountPath: TargetMountPath, ReadOnly: false},
							{Name: workVolumeName, MountPath: workMountPath, ReadOnly: false},
						},
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
				},
			},
		},
	}
}
