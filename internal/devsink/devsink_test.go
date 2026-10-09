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
	h := Handler(&log, wire.AgentUpdateTarget{})

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

// O devsink simula o contrato da api (api#325) no e2e: o alvo configurado é oferecido a
// quem não está nele, e o desfecho é validado e logado.
func TestHandlerServesAgentUpdateTarget(t *testing.T) {
	var log strings.Builder
	h := Handler(&log, wire.AgentUpdateTarget{Version: "0.1.1", ChartDigest: "sha256:abc"})
	path := wire.AgentUpdateTargetPath + "?agentVersion="

	if w, _ := agentUpdate(t, h, "GET", path+"0.1.0", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("sem Bearer: quer 401, veio %d", w.Code)
	}
	w, body := agentUpdate(t, h, "GET", path+"0.1.0", "", "Bearer tok")
	if w.Code != http.StatusOK || body != `{"version":"0.1.1","chartDigest":"sha256:abc"}`+"\n" {
		t.Fatalf("alvo: %d %q", w.Code, body)
	}
	if w, _ := agentUpdate(t, h, "GET", path+"0.1.1", "", "Bearer tok"); w.Code != http.StatusNoContent {
		t.Fatalf("já no alvo: quer 204, veio %d", w.Code)
	}
	empty := Handler(&log, wire.AgentUpdateTarget{})
	if w, _ := agentUpdate(t, empty, "GET", path+"0.1.0", "", "Bearer tok"); w.Code != http.StatusNoContent {
		t.Fatalf("sem alvo configurado: quer 204, veio %d", w.Code)
	}
}

func TestHandlerValidatesAndLogsAgentUpdateOutcome(t *testing.T) {
	var log strings.Builder
	h := Handler(&log, wire.AgentUpdateTarget{})
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"version":"0.1.1","outcome":"applied"}`, http.StatusNoContent},
		{`{"version":"0.1.2","outcome":"rolled_back","reason":"timeout"}`, http.StatusNoContent},
		{`{"version":"0.1.3","outcome":"rejected_by_ceiling","reason":"forbidden"}`, http.StatusNoContent},
		{`{"version":"0.1.3","outcome":"abstained","reason":"gitops_flux"}`, http.StatusNoContent},
		{`{"version":"0.1.2","outcome":"rolled_back"}`, http.StatusBadRequest},
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
