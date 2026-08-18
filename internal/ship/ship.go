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

// queued é uma janela JÁ serializada e comprimida. A fila guarda BYTES, não structs.
//
// Por quê: guardar []wire.Snapshot mantinha na heap o grafo inteiro de cada janela, e num
// cluster grande cada janela pesa ~2 MB contra limits.memory de 256Mi — as 12h nominais de
// buffer viravam ~1h antes do OOMKill. Comprimir na ENTRADA (o shipper já comprimia no
// envio; é só mover para cá) leva ~2 MB para ~250 KB, ou seja ~8× mais janelas na mesma
// memória, e permite limitar por bytes, que é a grandeza que de fato causa OOM.
type queued struct {
	windowStart time.Time
	body        []byte // JSON gzipado, pronto para o POST
}

// Shipper — invariante: Enqueue e Flush são chamados da MESMA goroutine (o select loop do main) — não há proteção TOCTOU entre eles além do mutex por operação.
type Shipper struct {
	url      string
	token    string
	max      int
	maxBytes int
	client   *http.Client

	mu      sync.Mutex
	queue   []queued
	bytes   int
	dropped int
}

// New monta o shipper. bufferWindows limita a fila em JANELAS e bufferBytes em BYTES
// comprimidos; vale o que estourar primeiro. O teto de bytes é o que protege de verdade —
// o de janelas não sabe quão grande é o cluster.
func New(url, token string, bufferWindows, bufferBytes int) *Shipper {
	return &Shipper{
		url: url, token: token, max: bufferWindows, maxBytes: bufferBytes,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Enqueue serializa, comprime e enfileira; cheia = descarta o MAIS ANTIGO (janela recente
// vale mais). Falha de serialização descarta a janela: ela não teria como ser enviada
// depois, e segurá-la só ocuparia a fila.
func (s *Shipper) Enqueue(snap wire.Snapshot) {
	body, err := encodeSnapshot(snap)
	if err != nil {
		slog.Error("encode failed, dropping window", "windowStart", snap.WindowStart, "err", err)
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, queued{windowStart: snap.WindowStart, body: body})
	s.bytes += len(body)
	for len(s.queue) > 0 && (len(s.queue) > s.max || (s.maxBytes > 0 && s.bytes > s.maxBytes)) {
		// Nunca descarta a janela recém-enfileirada até o fim: se ela sozinha estourar o
		// teto de bytes, o loop para com ela na fila (len==1) e o envio a rejeita ou aceita
		// — engolir a mais nova deixaria a fila vazia e o buraco invisível.
		if len(s.queue) == 1 {
			break
		}
		s.bytes -= len(s.queue[0].body)
		s.queue = append([]queued(nil), s.queue[1:]...)
		s.dropped++
		slog.Warn("buffer full, dropping oldest window", "queued", len(s.queue), "bytes", s.bytes)
	}
}

func (s *Shipper) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// PendingBytes é a ocupação comprimida da fila.
func (s *Shipper) PendingBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Dropped é o total de janelas perdidas desde a partida (buffer cheio ou encode falho).
// Antes elas sumiam com um log e mais nada — ninguém do nosso lado sabia que houve buraco.
func (s *Shipper) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// encodeSnapshot serializa e comprime — o mesmo formato que o POST espera.
func encodeSnapshot(snap wire.Snapshot) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(snap); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	return buf.Bytes(), nil
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
				"windowStart", next.windowStart)
		}
		s.mu.Lock()
		s.bytes -= len(s.queue[0].body)
		s.queue = s.queue[1:]
		s.mu.Unlock()
	}
}

// post envia UMA janela já serializada. transient=true quando vale reenviar (5xx/429/rede).
func (s *Shipper) post(ctx context.Context, q queued) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+wire.Path, bytes.NewReader(q.body))
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
