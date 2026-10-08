package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Falhas são acompanhadas por nó, mas exportadas sem labels de cardinalidade aberta.
// Um nó removido da lista esperada deixa de ser obrigação da coleta seguinte.
type collectionHealth struct {
	mu                                   sync.Mutex
	failures                             map[string]float64
	lastCompleted, lastSuccess, duration float64
	expected, collected                  int
}

func (h *collectionHealth) observe(nodes []*corev1.Node, samples []nodeSample, started time.Time) {
	completed := float64(time.Now().UnixNano()) / 1e9
	failures := make(map[string]float64, len(nodes))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, node := range nodes {
		since := h.failures[node.Name]
		if since == 0 {
			since = completed
		}
		failures[node.Name] = since
	}
	for _, sample := range samples {
		delete(failures, sample.node)
	}
	h.failures = failures
	h.expected, h.collected = len(nodes), len(samples)
	h.lastCompleted, h.duration = completed, time.Since(started).Seconds()
	if len(failures) == 0 {
		h.lastSuccess = completed
	}
}

func (h *collectionHealth) writeMetrics(w io.Writer) error {
	h.mu.Lock()
	expected, collected := h.expected, h.collected
	lastCompleted, lastSuccess, duration := h.lastCompleted, h.lastSuccess, h.duration
	failed, complete := 0, 0
	var since float64
	for _, at := range h.failures {
		if since == 0 || at < since {
			since = at
		}
	}
	if len(h.failures) > 0 {
		failed = 1
	}
	if lastCompleted > 0 && failed == 0 {
		complete = 1
	}
	h.mu.Unlock()
	_, err := fmt.Fprintf(w, `# HELP nuvemcash_agent_collection_failed Coleta com ao menos um nó esperado sem recuperação.
# TYPE nuvemcash_agent_collection_failed gauge
nuvemcash_agent_collection_failed %d
# HELP nuvemcash_agent_collection_failure_since_timestamp_seconds Início da falha corrente mais antiga entre os nós esperados, ou zero.
# TYPE nuvemcash_agent_collection_failure_since_timestamp_seconds gauge
nuvemcash_agent_collection_failure_since_timestamp_seconds %g
# HELP nuvemcash_agent_collection_complete Último ciclo concluído com todos os nós esperados.
# TYPE nuvemcash_agent_collection_complete gauge
nuvemcash_agent_collection_complete %d
# HELP nuvemcash_agent_collection_nodes_expected Nós esperados no último ciclo concluído.
# TYPE nuvemcash_agent_collection_nodes_expected gauge
nuvemcash_agent_collection_nodes_expected %d
# HELP nuvemcash_agent_collection_nodes_collected Nós coletados no último ciclo concluído.
# TYPE nuvemcash_agent_collection_nodes_collected gauge
nuvemcash_agent_collection_nodes_collected %d
# HELP nuvemcash_agent_collection_last_completed_timestamp_seconds Última conclusão de ciclo, inclusive parcial, ou zero.
# TYPE nuvemcash_agent_collection_last_completed_timestamp_seconds gauge
nuvemcash_agent_collection_last_completed_timestamp_seconds %g
# HELP nuvemcash_agent_collection_last_success_timestamp_seconds Última conclusão com todos os nós esperados, ou zero.
# TYPE nuvemcash_agent_collection_last_success_timestamp_seconds gauge
nuvemcash_agent_collection_last_success_timestamp_seconds %g
# HELP nuvemcash_agent_collection_duration_seconds Duração do último ciclo concluído.
# TYPE nuvemcash_agent_collection_duration_seconds gauge
nuvemcash_agent_collection_duration_seconds %g
`, failed, since, complete, expected, collected, lastCompleted, lastSuccess, duration)
	return err
}
