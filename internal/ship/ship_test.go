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

	s := New(srv.URL, "tok-1", 10)
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

	s := New(srv.URL, "tok", 10)
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
	s := New(srv.URL, "tok-revogado", 10)
	s.Enqueue(snap("c1"))
	_ = s.Flush(context.Background())
	if s.Pending() != 0 {
		t.Fatalf("401 devia descartar (token revogado não é transiente): %d", s.Pending())
	}
}

func TestEnqueue_BufferDescartaMaisAntigo(t *testing.T) {
	s := New("http://unused", "tok", 2)
	s.Enqueue(snap("a"))
	s.Enqueue(snap("b"))
	s.Enqueue(snap("c"))
	if s.Pending() != 2 {
		t.Fatalf("cap do buffer: %d", s.Pending())
	}
	if s.queue[0].ClusterUID != "b" || s.queue[1].ClusterUID != "c" {
		t.Fatalf("devia descartar o mais antigo: %+v", s.queue)
	}
}
