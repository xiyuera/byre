{{/*
Names. `fullname` is release-scoped so two releases in one namespace do not collide, but
every resource also gets a `nameOverride` escape hatch for teams that need fixed names.
*/}}
{{- define "byre.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "byre.fullname" -}}
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

{{- define "byre.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "byre.labels" -}}
helm.sh/chart: {{ include "byre.chart" . }}
{{ include "byre.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
{{- end -}}

{{- define "byre.selectorLabels" -}}
app.kubernetes.io/name: {{ include "byre.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "byre.serviceAccountName" -}}
{{- if .Values.rbac.serviceAccountName -}}
{{- .Values.rbac.serviceAccountName -}}
{{- else -}}
{{- include "byre.fullname" . -}}
{{- end -}}
{{- end -}}

{{/* Backend namespace defaults to the release namespace. */}}
{{- define "byre.backendNamespace" -}}
{{- default .Release.Namespace .Values.backend.namespace -}}
{{- end -}}

{{/* Resolved image reference. */}}
{{- define "byre.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/* Name of the secret holding the pool key: existing one, or the one this chart renders. */}}
{{- define "byre.poolKeySecret" -}}
{{- if .Values.poolKey.existingSecret -}}
{{- .Values.poolKey.existingSecret -}}
{{- else -}}
{{- printf "%s-pool-key" (include "byre.fullname" .) -}}
{{- end -}}
{{- end -}}
