{{- define "nuvemcash-agent.rules" -}}
groups:
  - name: {{ printf "%s-%s-agent" .Release.Namespace .Release.Name | quote }}
    rules:
      - alert: NuvemcashAgentCollectionFailed
        expr: |
          max by (cluster) (time() - ({{ include "nuvemcash-agent.metric" (dict "root" . "name" "nuvemcash_agent_collection_failure_since_timestamp_seconds") }} > 0)) >= {{ .Values.monitoring.alerts.failureSeconds }}
        labels:
          {{- include "nuvemcash-agent.alertLabels" (dict "root" . "condition" "collection_failed") | nindent 10 }}
        annotations:
          summary: "Coleta local incompleta por falha persistente"
          description: {{ printf "Ao menos um nó esperado permanece sem coleta há %v segundos ou mais; um envio parcial aceito não recupera essa condição." .Values.monitoring.alerts.failureSeconds | quote }}
          evidence: 'Consulte /metrics (collection_nodes_expected, collection_nodes_collected, collection_failure_since_timestamp_seconds) e os logs node scrape failed da release {{ "{{ $labels.release }}" }} no namespace {{ "{{ $labels.namespace }}" }}.'
          action: "Verifique o kubelet, a conectividade via apiserver e a permissão nodes/proxy. A recuperação exige nova coleta do nó afetado ou sua saída da lista esperada."
      - alert: NuvemcashAgentShipFailed
        expr: |
          max by (cluster) (time() - ({{ include "nuvemcash-agent.metric" (dict "root" . "name" "nuvemcash_agent_ship_failure_since_timestamp_seconds") }} > 0)) >= {{ .Values.monitoring.alerts.failureSeconds }}
        labels:
          {{- include "nuvemcash-agent.alertLabels" (dict "root" . "condition" "ship_failed") | nindent 10 }}
        annotations:
          summary: "Envio local impedido com dados retidos"
          description: {{ printf "Uma janela em memória permanece impedida de enviar há %v segundos ou mais. Fila vazia e retry recuperado não satisfazem a condição." .Values.monitoring.alerts.failureSeconds | quote }}
          evidence: 'Consulte /metrics (buffer_windows, buffer_bytes, buffer_oldest_age_seconds) e os logs ship failed da release {{ "{{ $labels.release }}" }} no namespace {{ "{{ $labels.namespace }}" }}.'
          action: "Verifique o endpoint de ingestão, a conectividade, os códigos 429/5xx e os logs. O aceite da janela afetada recupera seu envio; descarte é sinalizado separadamente."
      - alert: NuvemcashAgentDataLoss
        expr: |
          (time() - max by (cluster, reason) ({{ include "nuvemcash-agent.metric" (dict "root" . "name" "nuvemcash_agent_last_drop_timestamp_seconds") }} > 0)) < {{ .Values.monitoring.alerts.lossWindowSeconds }}
        labels:
          {{- include "nuvemcash-agent.alertLabels" (dict "root" . "condition" "data_loss") | nindent 10 }}
        annotations:
          summary: "Perda definitiva de janela do agente"
          description: 'Uma janela foi descartada recentemente por {{ "{{ $labels.reason }}" }}. O encerramento do aviso indica o fim da janela de observação, não restauração do dado.'
          evidence: 'Consulte /metrics (dropped_windows_total, last_drop_timestamp_seconds) e os logs com windowStart da release {{ "{{ $labels.release }}" }} no namespace {{ "{{ $labels.namespace }}" }}.'
          action: "Investigue buffer_windows/buffer_bytes, encode ou http_rejected. Corrija a causa indicada nos logs; o buffer em memória não permite reenviar uma janela já descartada."
{{- end -}}
