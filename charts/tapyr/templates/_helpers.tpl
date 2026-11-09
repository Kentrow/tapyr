{{/* The name of the resources: the release, with the chart name unless the release has it. */}}
{{- define "tapyr.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "tapyr.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "tapyr.labels" -}}
{{ include "tapyr.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* The configuration file: the Settings document, then the documents of the user. */}}
{{- define "tapyr.config" -}}
{{- $fixed := dict "server" (dict "listen" "0.0.0.0:9790") "state" (dict "path" "/home/nonroot/data/tapyr.db") -}}
{{- $settings := mustMergeOverwrite (deepCopy .Values.settings) $fixed -}}
apiVersion: tapyr/v1alpha1
kind: Settings
metadata:
  name: helm
spec:
{{ toYaml $settings | indent 2 }}
{{- with .Values.config }}
---
{{ . | trim }}
{{- end }}
{{- end -}}
