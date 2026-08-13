// Package ship entrega snapshots ao ingest com gzip, Bearer e fila limitada em memória.
// Perda em restart do pod é aceita por design (vira "não monitorado" no backend — spec 15).
package ship

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nuvemcash/agent/wire"
)

// Shipper — invariante: Enqueue e Flush são chamados da MESMA goroutine (o select loop do main) — não há proteção TOCTOU entre eles além do mutex por operação.
type Shipper struct {
	url    string
	token  string
	max    int
	client *http.Client

	mu    sync.Mutex
	queue []wire.Snapshot
}

func New(url, token string, bufferWindows int) *Shipper {
	return &Shipper{
		url: url, token: token, max: bufferWindows,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Enqueue adiciona à fila; cheia = descarta o MAIS ANTIGO (janela recente vale mais).
func (s *Shipper) Enqueue(snap wire.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, snap)
	if len(s.queue) > s.max {
		drop := len(s.queue) - s.max
		s.queue = append([]wire.Snapshot(nil), s.queue[drop:]...)
		slog.Warn("buffer full, dropping oldest windows", "dropped", drop)
	}
}

func (s *Shipper) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// Flush drena oldest-first; para no primeiro erro transiente (mantém o restante).
func (s *Shipper) Flush(ctx context.Context) error {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			return nil
		}
		next := s.queue[0]
		s.mu.Unlock()

		transient, err := s.post(ctx, next)
		if err != nil && transient {
			return err // mantém na fila; o loop de envio tenta de novo depois
		}
		if err != nil {
			slog.Error("snapshot rejected by ingest, dropping", "err", err,
				"windowStart", next.WindowStart)
		}
		s.mu.Lock()
		s.queue = s.queue[1:]
		s.mu.Unlock()
	}
}

// post envia UM snapshot. transient=true quando vale reenviar (5xx/429/rede).
func (s *Shipper) post(ctx context.Context, snap wire.Snapshot) (bool, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(snap); err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}
	if err := zw.Close(); err != nil {
		return false, fmt.Errorf("gzip: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+wire.Path, &buf)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return true, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // corpo já lido/descartado; erro de close não é acionável
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("ingest %d", resp.StatusCode)
	default: // 4xx definitivo (401 token revogado, 413 grande demais, ...)
		return false, fmt.Errorf("ingest %d", resp.StatusCode)
	}
}
