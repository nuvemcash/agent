{{- /* Imagem do agente. O updater usa a MESMA: a imagem dele sobe junto com a do coletor.
     Precedência: image.tag explícito > image.digest (gravado pelo release) > appVersion.
     Com digest a tag do appVersion acompanha (repo:tag@digest): o kubelet puxa pelo digest e a
     tag segue legível para a abstenção sob Argo (splitImage). */ -}}
{{- define "nuvemcash-agent.image" -}}
{{- if and .Values.image.digest (not .Values.image.tag) -}}
{{ .Values.image.repository }}:{{ .Chart.AppVersion }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

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
{{- $labels := mergeOverwrite (omit (deepCopy $root.Values.monitoring.alerts.additionalLabels) "cluster" "reason" "status" "installed_version" "latest_version") $identity (dict "product" "nuvemcash" "notification_mode" $root.Values.monitoring.alerts.notificationMode "condition" .condition "severity" (.severity | default "warning")) -}}
{{ toYaml $labels }}
{{- end -}}

{{- define "nuvemcash-agent.metric" -}}
{{- $selector := include "nuvemcash-agent.metricSelector" .root -}}
{{- if .status -}}{{- $selector = printf "%s,status=%q" $selector .status -}}{{- end -}}
{{- if .root.Values.monitoring.cluster -}}
{{ .name }}{ {{- $selector -}} }
{{- else -}}
({{ .name }}{ {{- $selector -}},cluster!=""} or label_replace({{ .name }}{ {{- $selector -}},cluster=""}, "cluster", "local", "", ""))
{{- end -}}
{{- end -}}
