package ship

import (
	"fmt"
	"io"
	"time"
)

const (
	dropBufferWindows = iota
	dropBufferBytes
	dropEncode
	dropHTTPRejected
)

var dropReasons = [...]string{"buffer_windows", "buffer_bytes", "encode", "http_rejected"}

type loss struct {
	count int
	last  time.Time
}

// recordDrop é chamado com o mutex da fila adquirido.
func (s *Shipper) recordDrop(reason int) {
	s.dropped++
	s.losses[reason].count++
	s.losses[reason].last = time.Now()
}

// WriteMetrics publica o estado da fila real; envio em vazio não produz sucesso.
func (s *Shipper) WriteMetrics(w io.Writer, installedVersion string) error {
	s.mu.Lock()
	pending, bytes := len(s.queue), s.bytes
	lastSuccess := timestamp(s.lastSuccess)
	losses := s.losses
	latestVersion := s.latestVersion
	var oldest, failedSince float64
	if pending > 0 {
		oldest = timestamp(s.queue[0].enqueuedAt)
		failedSince = timestamp(s.queue[0].failedSince)
	}
	s.mu.Unlock()
	failed, age := 0, 0.0
	if failedSince > 0 {
		failed = 1
	}
	if oldest > 0 {
		age = max(0, float64(time.Now().UnixNano())/1e9-oldest)
	}
	if _, err := fmt.Fprintf(w, `# HELP nuvemcash_agent_ship_failed Envio impedido por falha de transporte ainda não recuperada.
# TYPE nuvemcash_agent_ship_failed gauge
nuvemcash_agent_ship_failed %d
# HELP nuvemcash_agent_ship_failure_since_timestamp_seconds Início da falha da janela retida, ou zero.
# TYPE nuvemcash_agent_ship_failure_since_timestamp_seconds gauge
nuvemcash_agent_ship_failure_since_timestamp_seconds %g
# HELP nuvemcash_agent_ship_last_success_timestamp_seconds Última janela aceita por HTTP, ou zero.
# TYPE nuvemcash_agent_ship_last_success_timestamp_seconds gauge
nuvemcash_agent_ship_last_success_timestamp_seconds %g
# HELP nuvemcash_agent_buffer_windows Janelas aguardando envio em memória.
# TYPE nuvemcash_agent_buffer_windows gauge
nuvemcash_agent_buffer_windows %d
# HELP nuvemcash_agent_buffer_bytes Bytes comprimidos aguardando envio em memória.
# TYPE nuvemcash_agent_buffer_bytes gauge
nuvemcash_agent_buffer_bytes %d
# HELP nuvemcash_agent_buffer_oldest_age_seconds Tempo de espera da janela enfileirada mais antiga.
# TYPE nuvemcash_agent_buffer_oldest_age_seconds gauge
nuvemcash_agent_buffer_oldest_age_seconds %g
`, failed, failedSince, lastSuccess, pending, bytes, age); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, `# HELP nuvemcash_agent_dropped_windows_total Janelas definitivamente perdidas desde a partida por motivo limitado.
# TYPE nuvemcash_agent_dropped_windows_total counter
`); err != nil {
		return err
	}
	for reason, loss := range losses {
		if _, err := fmt.Fprintf(w, "nuvemcash_agent_dropped_windows_total{reason=%q} %d\n", dropReasons[reason], loss.count); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, `# HELP nuvemcash_agent_last_drop_timestamp_seconds Instante da última perda definitiva por motivo, ou zero; não sobrevive ao processo.
# TYPE nuvemcash_agent_last_drop_timestamp_seconds gauge
`); err != nil {
		return err
	}
	for reason, loss := range losses {
		if _, err := fmt.Fprintf(w, "nuvemcash_agent_last_drop_timestamp_seconds{reason=%q} %g\n", dropReasons[reason], timestamp(loss.last)); err != nil {
			return err
		}
	}
	return writeVersionMetrics(w, installedVersion, latestVersion)
}

func timestamp(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}
