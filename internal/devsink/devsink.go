// Package devsink é o receptor de DESENVOLVIMENTO usado no e2e da Fase 2 — o ingest real
// (autenticado, persistente) é a Fase 3 no backend do nuvem.cash. Aceita o contrato wire,
// responde 202 e loga um resumo legível por snapshot. Também simula o contrato da
// atualização automática (api#325): oferece um alvo fixo e valida e loga os desfechos.
package devsink

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nuvemcash/agent/wire"
)

// Handler monta o receptor. target vazio = a api não tem nada a oferecer (204).
func Handler(out io.Writer, target wire.AgentUpdateTarget) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.AgentUpdateTargetPath, func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		installed := strings.TrimPrefix(r.URL.Query().Get("agentVersion"), "v")
		_, _ = fmt.Fprintf(out, "agent-update target agentVersion=%s offered=%s\n", installed, target.Version)
		if target.Version == "" || target.Version == installed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(target)
	})
	mux.HandleFunc("POST "+wire.AgentUpdateOutcomePath, func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		var rep wire.AgentUpdateReport
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil || !validReport(rep) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"INVALID_OUTCOME"}`)
			return
		}
		_, _ = fmt.Fprintf(out, "agent-update outcome version=%s outcome=%s reason=%q\n", rep.Version, rep.Outcome, rep.Reason)
		w.WriteHeader(http.StatusNoContent)
	})
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
		var sampled int64
		for _, n := range s.Nodes {
			sampled += n.SampledSeconds
		}
		// start/end/sampled em segundos Unix: o e2e confere com eles que a troca de pods
		// na atualização não mede os mesmos segundos duas vezes.
		_, _ = fmt.Fprintf(out, "snapshot cluster=%s agent=%s window=%s start=%d end=%d sampled=%d nodes=%d usage=%d pvcs=%d\n",
			s.ClusterUID, s.AgentVersion, s.WindowStart.Format("15:04:05"), s.WindowStart.Unix(), s.WindowEnd.Unix(),
			sampled, len(s.Nodes), len(s.Usage), len(s.PVCs))
		for _, u := range s.Usage {
			_, _ = fmt.Fprintf(out, "  %s/%s (%s) node=%s cpuΔ=%.3fcore·s cobertura=%ds\n",
				u.Namespace, u.WorkloadName, u.WorkloadKind, u.Node, u.CPUUsageCoreSeconds, u.CoverageSeconds)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

// bearer espelha a api: sem "Authorization: Bearer <token>" é 401 INVALID_TOKEN.
func bearer(w http.ResponseWriter, r *http.Request) bool {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"code":"INVALID_TOKEN"}`)
	return false
}

// validReport aplica as regras do relato da api (cluster.AgentUpdateReport.Normalize).
func validReport(r wire.AgentUpdateReport) bool {
	if r.Version == "" {
		return false
	}
	switch r.Outcome {
	case wire.OutcomeApplied, wire.OutcomeRejectedByCeiling:
		return true
	case wire.OutcomeRolledBack:
		return strings.TrimSpace(r.Reason) != ""
	case wire.OutcomeAbstained:
		switch r.Reason {
		case "gitops_flux", "gitops_argo", "mirrored_registry", "disabled":
			return true
		}
	}
	return false
}
