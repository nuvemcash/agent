package update_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nuvemcash/agent/internal/devsink"
	"github.com/nuvemcash/agent/internal/update"
	"github.com/nuvemcash/agent/wire"
)

// O devsink valida o relato com as regras da api; as primeiras tentativas caem em 503,
// como uma api fora do ar na partida do coletor.
func TestReportDisabledRetriesUntilAPIAccepts(t *testing.T) {
	var out bytes.Buffer
	sink := devsink.Handler(&out, nil)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		sink.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api := update.API{URL: srv.URL, Token: token, Client: srv.Client()}
	if err := update.ReportDisabled(ctx, api, "v0.6.0", time.Millisecond); err != nil {
		t.Fatalf("ReportDisabled: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("tentativas = %d, quer 3", calls.Load())
	}
	want := `agent-update outcome version=0.6.0 outcome=` + string(wire.OutcomeAbstained) + ` reason="` + wire.ReasonDisabled + `"`
	if !strings.Contains(out.String(), want) {
		t.Fatalf("relato não chegou como abstenção por desligamento: %q", out.String())
	}
}

// Build de desenvolvimento não tem versão de release, e a api recusaria o relato para sempre.
func TestReportDisabledSkipsNonReleaseVersion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	api := update.API{URL: srv.URL, Token: token, Client: srv.Client()}
	if err := update.ReportDisabled(context.Background(), api, "dev", time.Millisecond); err != nil {
		t.Fatalf("ReportDisabled: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("versão dev não deve ser relatada, houve %d chamadas", calls.Load())
	}
}

func TestReportDisabledStopsWithContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	api := update.API{URL: srv.URL, Token: token, Client: srv.Client()}
	if err := update.ReportDisabled(ctx, api, "0.6.0", time.Millisecond); err == nil {
		t.Fatal("api sempre fora: esperava o erro do contexto")
	}
}
