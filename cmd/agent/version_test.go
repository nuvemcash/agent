package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nuvemcash/agent/internal/ship"
	"github.com/nuvemcash/agent/wire"
)

// Fixtures literais do contrato API docs/contracts/k8s-ingest.md (T5 f6216828).
func TestLocalVersionAfterHTTPAcceptance(t *testing.T) {
	previous := version
	t.Cleanup(func() { version = previous })
	for _, tc := range []struct {
		name, installed, body, status, installedLabel, latestLabel string
		incomplete                                                 bool
	}{
		{name: "desatualizado", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0"}`, status: "outdated", installedLabel: "v0.9.0", latestLabel: "v0.10.0"},
		{name: "prefixo convencional", installed: "v0.9.0", body: `{"latestAgentVersion":"v0.10.0"}`, status: "outdated", installedLabel: "v0.9.0", latestLabel: "v0.10.0"},
		{name: "atualizado", installed: "0.10.0", body: `{"latestAgentVersion":"0.10.0"}`, status: "updated", installedLabel: "v0.10.0", latestLabel: "v0.10.0"},
		{name: "à frente", installed: "0.11.0", body: `{"latestAgentVersion":"0.10.0"}`, status: "updated", installedLabel: "v0.11.0", latestLabel: "v0.10.0"},
		{name: "pré-release", installed: "0.10.0-rc.1", body: `{"latestAgentVersion":"0.10.0"}`, status: "outdated", installedLabel: "v0.10.0-rc.1", latestLabel: "v0.10.0"},
		{name: "ordem numérica pré-release", installed: "0.10.0-rc.2", body: `{"latestAgentVersion":"0.10.0-rc.11"}`, status: "outdated", installedLabel: "v0.10.0-rc.2", latestLabel: "v0.10.0-rc.11"},
		{name: "release posterior ao preview", installed: "0.10.0", body: `{"latestAgentVersion":"0.10.0-rc.1"}`, status: "updated", installedLabel: "v0.10.0", latestLabel: "v0.10.0-rc.1"},
		{name: "build metadata não altera precedência nem labels", installed: "0.10.0+local", body: `{"latestAgentVersion":"0.10.0+remote"}`, status: "updated", installedLabel: "v0.10.0", latestLabel: "v0.10.0"},
		{name: "build dev", installed: "dev", body: `{"latestAgentVersion":"0.10.0"}`, status: "unknown", latestLabel: "v0.10.0"},
		{name: "build vazio", body: `{"latestAgentVersion":"0.10.0"}`, status: "unknown", latestLabel: "v0.10.0"},
		{name: "api antiga vazia", installed: "0.9.0", status: "unknown", installedLabel: "v0.9.0"},
		{name: "api antiga objeto vazio", installed: "0.9.0", body: `{}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "referência vazia", installed: "0.9.0", body: `{"latestAgentVersion":""}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "referência dev", installed: "0.9.0", body: `{"latestAgentVersion":"dev"}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "referência inválida", installed: "0.9.0", body: `{"latestAgentVersion":"0.010.0"}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "null", installed: "0.9.0", body: `{"latestAgentVersion":null}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "número", installed: "0.9.0", body: `{"latestAgentVersion":123}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "objeto", installed: "0.9.0", body: `{"latestAgentVersion":{}}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "array", installed: "0.9.0", body: `{"latestAgentVersion":[]}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "booleano", installed: "0.9.0", body: `{"latestAgentVersion":true}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "documento null", installed: "0.9.0", body: `null`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "json corrompido", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0"`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "documento extra", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0"}{}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "leitura incompleta", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0"}`, incomplete: true, status: "unknown", installedLabel: "v0.9.0"},
		{name: "corpo no limite", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0"}` + strings.Repeat(" ", 4096-len(`{"latestAgentVersion":"0.10.0"}`)), status: "outdated", installedLabel: "v0.9.0", latestLabel: "v0.10.0"},
		{name: "corpo acima do limite", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0"}` + strings.Repeat(" ", 4097-len(`{"latestAgentVersion":"0.10.0"}`)), status: "unknown", installedLabel: "v0.9.0"},
		{name: "versão acima do limite", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0-` + strings.Repeat("a", 121) + `"}`, status: "unknown", installedLabel: "v0.9.0"},
		{name: "versão no limite", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0-` + strings.Repeat("a", 120) + `"}`, status: "outdated", installedLabel: "v0.9.0", latestLabel: "v0.10.0-" + strings.Repeat("a", 120)},
		{name: "build acima do limite", installed: "0.9.0-" + strings.Repeat("a", 200), body: `{"latestAgentVersion":"0.10.0"}`, status: "unknown", latestLabel: "v0.10.0"},
		{name: "metadado adicional tolerado", installed: "0.9.0", body: `{"latestAgentVersion":"0.10.0","future":true}`, status: "outdated", installedLabel: "v0.9.0", latestLabel: "v0.10.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.installed
			var attempts atomic.Int32
			ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != wire.Path || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Content-Encoding") != "gzip" {
					t.Error("contrato do envio alterado")
				}
				zr, err := gzip.NewReader(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				defer zr.Close() //nolint:errcheck // leitor de fixture em memória
				var received wire.Snapshot
				if err := json.NewDecoder(zr).Decode(&received); err != nil || received.AgentVersion != tc.installed {
					t.Errorf("snapshot não preservado: %+v, %v", received, err)
				}
				body := `{"latestAgentVersion":"0.20.0"}`
				if attempts.Add(1) > 1 {
					body = tc.body
					if tc.incomplete {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)+10))
					}
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, body)
			}))
			defer ingest.Close()
			s := ship.New(ingest.URL, "tok", 10, 0)
			var ready atomic.Bool
			mux := probeMux(&ready, s, &collectionHealth{}, true)
			scrape := func() string {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("metrics status: %d", rec.Code)
				}
				return rec.Body.String()
			}
			if metricValue(t, scrape(), `nuvemcash_agent_version_status{status="unknown",installed_version="`+tc.installedLabel+`",latest_version=""}`) != 1 {
				t.Fatal("partida deve ser desconhecida")
			}
			for i := 0; i < 2; i++ {
				s.Enqueue(wire.Snapshot{AgentVersion: version})
				if err := s.Flush(t.Context()); err != nil {
					t.Fatalf("metadado opcional não pode invalidar aceite: %v", err)
				}
				if i == 0 && tc.installedLabel != "" && metricValue(t, scrape(), `nuvemcash_agent_version_status{status="outdated",installed_version="`+tc.installedLabel+`",latest_version="v0.20.0"}`) != 1 {
					t.Fatal("primeira referência deve ser conhecida")
				}
			}
			body := scrape()
			expected := `nuvemcash_agent_version_status{status="` + tc.status + `",installed_version="` + tc.installedLabel + `",latest_version="` + tc.latestLabel + `"}`
			if metricValue(t, body, expected) != 1 || strings.Count(body, "\nnuvemcash_agent_version_status{") != 1 {
				t.Fatal("somente a comparação corrente deve ser exposta")
			}
			if err := s.Flush(t.Context()); err != nil || attempts.Load() != 2 || s.Pending() != 0 || s.Dropped() != 0 || metricValue(t, body, "nuvemcash_agent_ship_failed") != 0 {
				t.Fatalf("snapshot aceito não deve ser reenviado nem perdido: %v", err)
			}
		})
	}
}
