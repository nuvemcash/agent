package devsink

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nuvemcash/agent/wire"
)

func TestHandler_AceitaGzipELogaResumo(t *testing.T) {
	var log strings.Builder
	h := Handler(&log, nil)

	snap := wire.Snapshot{SchemaVersion: 1, ClusterUID: "c1", AgentVersion: "0.1.0",
		WindowStart: time.Unix(0, 0).UTC(), WindowEnd: time.Unix(300, 0).UTC(),
		Nodes: []wire.Node{{Name: "n1", SampledSeconds: 240}, {Name: "n2", SampledSeconds: 60}},
		Usage: []wire.WorkloadUsage{{Namespace: "app", WorkloadKind: "Deployment", WorkloadName: "web"}}}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = json.NewEncoder(zw).Encode(snap)
	_ = zw.Close()

	req := httptest.NewRequest("POST", wire.Path, &buf)
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 202 {
		t.Fatalf("esperava 202, veio %d", w.Code)
	}
	out := log.String()
	for _, want := range []string{"c1", "agent=0.1.0", "start=0", "end=300", "sampled=300", "nodes=2", "usage=1", "app/web"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log sem %q: %s", want, out)
		}
	}
}

func agentUpdate(t *testing.T, h http.Handler, method, path, body, auth string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w, w.Body.String()
}

// O devsink simula o contrato da api (api#325) no e2e: oferece a primeira versão do
// catálogo acima da instalada que não falhou neste cluster, e valida e loga o desfecho.
func TestHandlerServesAgentUpdateTarget(t *testing.T) {
	var log strings.Builder
	h := Handler(&log, []wire.AgentUpdateTarget{
		{Version: "0.1.1", ChartDigest: "sha256:a"}, {Version: "0.1.2", ChartDigest: "sha256:b"},
	})
	path := wire.AgentUpdateTargetPath + "?agentVersion="
	target := func(installed string) (int, string) {
		w, body := agentUpdate(t, h, "GET", path+installed, "", "Bearer tok")
		return w.Code, body
	}

	if w, _ := agentUpdate(t, h, "GET", path+"0.1.0", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("sem Bearer: quer 401, veio %d", w.Code)
	}
	if code, body := target("0.1.0"); code != http.StatusOK || body != `{"version":"0.1.1","chartDigest":"sha256:a"}`+"\n" {
		t.Fatalf("alvo: %d %q", code, body)
	}
	if code, body := target("v0.1.1"); code != http.StatusOK || !strings.Contains(body, `"0.1.2"`) {
		t.Fatalf("próxima versão: %d %q", code, body)
	}
	agentUpdate(t, h, "POST", wire.AgentUpdateOutcomePath, `{"version":"0.1.2","outcome":"rolled_back","reason":"x"}`, "Bearer tok")
	if code, _ := target("0.1.1"); code != http.StatusNoContent {
		t.Fatalf("versão que falhou não volta a ser oferecida: %d", code)
	}
	agentUpdate(t, h, "POST", wire.AgentUpdateOutcomePath, `{"version":"0.1.1","outcome":"signature_invalid","reason":"x"}`, "Bearer tok")
	if code, _ := target("0.1.0"); code != http.StatusNoContent {
		t.Fatalf("versão com assinatura inválida não volta a ser oferecida: %d", code)
	}
	if w, _ := agentUpdate(t, Handler(&log, nil), "GET", path+"0.1.0", "", "Bearer tok"); w.Code != http.StatusNoContent {
		t.Fatalf("catálogo vazio: quer 204, veio %d", w.Code)
	}
}

func TestHandlerValidatesAndLogsAgentUpdateOutcome(t *testing.T) {
	var log strings.Builder
	h := Handler(&log, nil)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"version":"0.1.1","outcome":"applied"}`, http.StatusNoContent},
		{`{"version":"0.1.2","outcome":"rolled_back","reason":"timeout"}`, http.StatusNoContent},
		{`{"version":"0.1.3","outcome":"rejected_by_ceiling","reason":"forbidden"}`, http.StatusNoContent},
		{`{"version":"0.1.3","outcome":"abstained","reason":"gitops_flux"}`, http.StatusNoContent},
		{`{"version":"0.1.4","outcome":"signature_invalid","reason":"SAN mismatch"}`, http.StatusNoContent},
		{`{"version":"0.1.2","outcome":"rolled_back"}`, http.StatusBadRequest},
		{`{"version":"0.1.4","outcome":"signature_invalid"}`, http.StatusBadRequest},
		{`{"version":"0.1.3","outcome":"abstained","reason":"cansei"}`, http.StatusBadRequest},
		{`{"version":"0.1.3","outcome":"done"}`, http.StatusBadRequest},
		{`{"version":"","outcome":"applied"}`, http.StatusBadRequest},
	} {
		if w, _ := agentUpdate(t, h, "POST", wire.AgentUpdateOutcomePath, c.body, "Bearer tok"); w.Code != c.want {
			t.Errorf("%s: quer %d, veio %d", c.body, c.want, w.Code)
		}
	}
	if w, _ := agentUpdate(t, h, "POST", wire.AgentUpdateOutcomePath, `{"version":"0.1.1","outcome":"applied"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("sem Bearer: quer 401, veio %d", w.Code)
	}
	for _, want := range []string{"outcome version=0.1.1 outcome=applied", `outcome version=0.1.2 outcome=rolled_back reason="timeout"`} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log sem %q:\n%s", want, log.String())
		}
	}
}
