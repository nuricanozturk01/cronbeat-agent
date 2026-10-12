{{- define "cronbeat-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cronbeat-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "cronbeat-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cronbeat-agent.labels" -}}
helm.sh/chart: {{ include "cronbeat-agent.chart" . }}
{{ include "cronbeat-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "cronbeat-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cronbeat-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "cronbeat-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cronbeat-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Name of the Secret holding the API key. */}}
{{- define "cronbeat-agent.secretName" -}}
{{- default (include "cronbeat-agent.fullname" .) .Values.existingSecret }}
{{- end }}

{{/* Key inside the Secret. */}}
{{- define "cronbeat-agent.secretKey" -}}
{{- if .Values.existingSecret }}{{ .Values.existingSecretKey }}{{ else }}api-key{{ end }}
{{- end }}

{{/* Read-only rules shared by the ClusterRole and the per-namespace Roles. */}}
{{- define "cronbeat-agent.rules" -}}
- apiGroups: ["batch"]
  resources: ["cronjobs", "jobs"]
  verbs: ["list", "watch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list", "watch"]
{{- end }}
