{{/*
Name helpers. The driver name itself is deliberately NOT derived from any of
these: it is a compile-time constant that is recorded in every PersistentVolume,
so it must never vary with the release name.
*/}}

{{- define "truenas-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "truenas-csi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* driverName is immutable: changing it orphans every existing PersistentVolume. */}}
{{- define "truenas-csi.driverName" -}}
csi.truenas.watteel.com
{{- end -}}

{{- define "truenas-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "truenas-csi.labels" -}}
helm.sh/chart: {{ include "truenas-csi.chart" . }}
{{ include "truenas-csi.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: truenas-csi
{{- end -}}

{{- define "truenas-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "truenas-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "truenas-csi.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* secretName is the single Secret holding the driver config, including every
API key. Both plugins mount it; RBAC grants `get` on this name only. */}}
{{- define "truenas-csi.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-config" (include "truenas-csi.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* loggingConfigMapName holds the live log level and format. It is separate
from the credential Secret on purpose: raising verbosity during an incident
must not require access to an API key. */}}
{{- define "truenas-csi.loggingConfigMapName" -}}
{{- if .Values.dynamicLogging.existingConfigMap -}}
{{- .Values.dynamicLogging.existingConfigMap -}}
{{- else -}}
{{- printf "%s-logging" (include "truenas-csi.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* metricsLeaseName is the Lease coordinating which controller replica polls
the appliance for array metrics. It is per release, so two releases in one
namespace do not fight over one lease. */}}
{{- define "truenas-csi.metricsLeaseName" -}}
{{- printf "%s-array-metrics" (include "truenas-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "truenas-csi.controllerServiceAccountName" -}}
{{- if .Values.serviceAccounts.controller.create -}}
{{- default (printf "%s-controller" (include "truenas-csi.fullname" .)) .Values.serviceAccounts.controller.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccounts.controller.name -}}
{{- end -}}
{{- end -}}

{{- define "truenas-csi.nodeServiceAccountName" -}}
{{- if .Values.serviceAccounts.node.create -}}
{{- default (printf "%s-node" (include "truenas-csi.fullname" .)) .Values.serviceAccounts.node.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccounts.node.name -}}
{{- end -}}
{{- end -}}

{{/* imagePullSecrets block, shared by both workloads. */}}
{{- define "truenas-csi.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
