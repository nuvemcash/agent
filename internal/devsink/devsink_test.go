package devsink

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nuvemcash/agent/wire"
)

func TestHandler_AceitaGzipELogaResumo(t *testing.T) {
	var log strings.Builder
	h := Handler(&log)

	snap := wire.Snapshot{SchemaVersion: 1, ClusterUID: "c1",
		WindowStart: time.Unix(0, 0).UTC(), WindowEnd: time.Unix(300, 0).UTC(),
		Nodes: []wire.Node{{Name: "n1"}},
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
	for _, want := range []string{"c1", "nodes=1", "usage=1", "app/web"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log sem %q: %s", want, out)
		}
	}
}
