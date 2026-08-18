package ship

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nuvemcash/agent/wire"
)

func snap(uid string) wire.Snapshot {
	return wire.Snapshot{SchemaVersion: 1, ClusterUID: uid,
		WindowStart: time.Unix(0, 0).UTC(), WindowEnd: time.Unix(300, 0).UTC()}
}

func TestFlush_SucessoDrenaEDecodificaGzip(t *testing.T) {
	var got wire.Snapshot
	var auth, enc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, enc = r.Header.Get("Authorization"), r.Header.Get("Content-Encoding")
		zr, _ := gzip.NewReader(r.Body)
		body, _ := io.ReadAll(zr)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s := New(srv.URL, "tok-1", 10, 0)
	s.Enqueue(snap("c1"))
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if s.Pending() != 0 || got.ClusterUID != "c1" || auth != "Bearer tok-1" || enc != "gzip" {
		t.Fatalf("entrega errada: pending=%d got=%+v auth=%q enc=%q", s.Pending(), got, auth, enc)
	}
}

func TestFlush_5xxMantemNaFila(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s := New(srv.URL, "tok", 10, 0)
	s.Enqueue(snap("c1"))
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("esperava erro no 500")
	}
	if s.Pending() != 1 {
		t.Fatalf("500 devia manter na fila: %d", s.Pending())
	}
	if err := s.Flush(context.Background()); err != nil || s.Pending() != 0 {
		t.Fatalf("retry devia drenar: %v %d", err, s.Pending())
	}
}

func TestFlush_401Descarta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := New(srv.URL, "tok-revogado", 10, 0)
	s.Enqueue(snap("c1"))
	_ = s.Flush(context.Background())
	if s.Pending() != 0 {
		t.Fatalf("401 devia descartar (token revogado não é transiente): %d", s.Pending())
	}
}

func TestEnqueue_BufferDescartaMaisAntigo(t *testing.T) {
	s := New("http://unused", "tok", 2, 0)
	s.Enqueue(snap("a"))
	s.Enqueue(snap("b"))
	s.Enqueue(snap("c"))
	if s.Pending() != 2 {
		t.Fatalf("cap do buffer: %d", s.Pending())
	}
	// A fila guarda BYTES agora, então a identidade da janela se lê pelo windowStart.
	if s.queue[0].windowStart != snap("b").WindowStart || len(s.queue[0].body) == 0 {
		t.Fatalf("devia descartar o mais antigo: %+v", s.queue)
	}
	if s.Dropped() != 1 {
		t.Fatalf("descarte devia ser contado: %d", s.Dropped())
	}
}

// O teto de BYTES é o que protege de verdade: o de janelas não sabe quão grande é o
// cluster, e foi o que deixou as 12h nominais de buffer virarem ~1h antes do OOMKill.
func TestEnqueue_TetoDeBytesDescartaMaisAntigo(t *testing.T) {
	s := New("http://unused", "tok", 1000, 1) // 1 byte: qualquer janela já estoura
	s.Enqueue(snap("a"))
	s.Enqueue(snap("b"))

	if s.Pending() != 1 {
		t.Fatalf("teto de bytes não segurou a fila: %d janelas", s.Pending())
	}
	if s.queue[0].windowStart != snap("b").WindowStart {
		t.Fatal("devia ter ficado com a janela MAIS RECENTE")
	}
}

// Uma janela que sozinha estoura o teto NÃO é engolida: descartá-la deixaria a fila vazia
// e o buraco invisível. Ela fica e o envio decide (o ingest responde 413 se for o caso).
func TestEnqueue_JanelaUnicaAcimaDoTetoNaoSomeSilenciosamente(t *testing.T) {
	s := New("http://unused", "tok", 1000, 1)
	s.Enqueue(snap("a"))

	if s.Pending() != 1 {
		t.Fatalf("janela única não podia ser descartada: %d", s.Pending())
	}
	if s.PendingBytes() == 0 {
		t.Fatal("ocupação em bytes devia ser contada")
	}
}
