// Package devsink é o receptor de DESENVOLVIMENTO usado no e2e da Fase 2 — o ingest real
// (autenticado, persistente) é a Fase 3 no backend do nuvem.cash. Aceita o contrato wire,
// responde 202 e loga um resumo legível por snapshot.
package devsink

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/nuvemcash/agent/wire"
)

func Handler(out io.Writer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.Path, func(w http.ResponseWriter, r *http.Request) {
		body := r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "gzip inválido", http.StatusBadRequest)
				return
			}
			defer zr.Close() //nolint:errcheck // corpo já decodificado; erro de close não é acionável
			body = zr
		}
		var s wire.Snapshot
		if err := json.NewDecoder(body).Decode(&s); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(out, "snapshot cluster=%s window=%s nodes=%d usage=%d pvcs=%d\n",
			s.ClusterUID, s.WindowStart.Format("15:04:05"), len(s.Nodes), len(s.Usage), len(s.PVCs))
		for _, u := range s.Usage {
			_, _ = fmt.Fprintf(out, "  %s/%s (%s) node=%s cpuΔ=%.3fcore·s cobertura=%ds\n",
				u.Namespace, u.WorkloadName, u.WorkloadKind, u.Node, u.CPUUsageCoreSeconds, u.CoverageSeconds)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}
