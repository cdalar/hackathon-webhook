{{- define "hackathon-webhook.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "hackathon-webhook" .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "hackathon-webhook.selectorLabels" -}}
app.kubernetes.io/name: {{ default "hackathon-webhook" .Values.nameOverride }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hackathon-webhook.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "hackathon-webhook.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "hackathon-webhook.secretName" -}}
{{- default (include "hackathon-webhook.fullname" .) .Values.existingSecret -}}
{{- end -}}
