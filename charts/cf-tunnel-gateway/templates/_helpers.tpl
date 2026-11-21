{{- define "cf-tunnel-gateway.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cf-tunnel-gateway.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "cf-tunnel-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cf-tunnel-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "cf-tunnel-gateway.labels" -}}
{{ include "cf-tunnel-gateway.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "cf-tunnel-gateway.secretName" -}}
{{- .Values.cloudflare.existingSecret | default (include "cf-tunnel-gateway.fullname" .) -}}
{{- end -}}

{{- define "cf-tunnel-gateway.instance" -}}
{{- .Values.instance | default "cf-tunnel-gateway" -}}
{{- end -}}

{{- define "cf-tunnel-gateway.controllerName" -}}
{{- $i := include "cf-tunnel-gateway.instance" . -}}
{{- if .Values.controller.name -}}
{{- .Values.controller.name -}}
{{- else if eq $i "cf-tunnel-gateway" -}}
cf-tunnel-gateway.davy.tw/gateway-controller
{{- else -}}
{{- printf "cf-tunnel-gateway.davy.tw/gateway-controller-%s" $i -}}
{{- end -}}
{{- end -}}
