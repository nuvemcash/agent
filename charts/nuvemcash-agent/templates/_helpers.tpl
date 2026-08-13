{{- define "nuvemcash-agent.secretName" -}}
{{- if .Values.connection.existingSecret -}}
{{ .Values.connection.existingSecret }}
{{- else -}}
{{ .Release.Name }}-token
{{- end -}}
{{- end -}}
