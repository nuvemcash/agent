{{- define "nuvemcash-agent.secretName" -}}
{{- if .Values.connection.existingSecret -}}
{{ .Values.connection.existingSecret }}
{{- else -}}
{{ .Release.Name }}-token
{{- end -}}
{{- end -}}

{{- define "nuvemcash-agent.monitoringIdentity" -}}
{{- if .Values.monitoring.cluster }}
cluster: {{ .Values.monitoring.cluster | quote }}
{{- end }}
namespace: {{ .Release.Namespace | quote }}
release: {{ .Release.Name | quote }}
environment: {{ .Values.monitoring.environment | quote }}
component: agent
{{- end -}}

{{- define "nuvemcash-agent.metricSelector" -}}
{{- if .Values.monitoring.cluster -}}{{ printf "cluster=%q," .Values.monitoring.cluster }}{{- end -}}
{{- printf "namespace=%q,release=%q,environment=%q,component=\"agent\"" .Release.Namespace .Release.Name .Values.monitoring.environment -}}
{{- end -}}

{{- define "nuvemcash-agent.alertLabels" -}}
{{- $root := .root -}}
{{- $identity := include "nuvemcash-agent.monitoringIdentity" $root | fromYaml -}}
{{- $labels := mergeOverwrite (deepCopy $root.Values.monitoring.alerts.additionalLabels) $identity (dict "product" "nuvemcash" "notification_mode" $root.Values.monitoring.alerts.notificationMode "condition" .condition "severity" "warning") -}}
{{ toYaml $labels }}
{{- end -}}

{{- define "nuvemcash-agent.metric" -}}
{{- $selector := include "nuvemcash-agent.metricSelector" .root -}}
{{- if .root.Values.monitoring.cluster -}}
{{ .name }}{ {{- $selector -}} }
{{- else -}}
({{ .name }}{ {{- $selector -}},cluster!=""} or label_replace({{ .name }}{ {{- $selector -}},cluster=""}, "cluster", "local", "", ""))
{{- end -}}
{{- end -}}
