package ship

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"golang.org/x/mod/semver"
)

const maxVersionBodyBytes = 4096

// Metadado opcional nunca desfaz o aceite HTTP, mesmo com leitura incompleta.
func (s *Shipper) observeVersion(body io.Reader) {
	var metadata struct {
		LatestAgentVersion string `json:"latestAgentVersion"`
	}
	latest := ""
	data, err := io.ReadAll(io.LimitReader(body, maxVersionBodyBytes+1))
	if err == nil && len(data) <= maxVersionBodyBytes && json.Unmarshal(data, &metadata) == nil {
		latest = normalizeVersion(metadata.LatestAgentVersion)
	}
	s.mu.Lock()
	s.latestVersion = latest
	s.mu.Unlock()
}

// Mesma normalização do produto; canonical remove build metadata dos labels.
func normalizeVersion(v string) string {
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if len(v) > 128 {
		return ""
	}
	return semver.Canonical(v)
}

func writeVersionMetrics(w io.Writer, installed, latest string) error {
	installed = normalizeVersion(installed)
	status := "unknown"
	if installed != "" && latest != "" {
		status = "updated"
		if semver.Compare(installed, latest) < 0 {
			status = "outdated"
		}
	}
	_, err := fmt.Fprintf(w, `# HELP nuvemcash_agent_version_status Comparação local com a referência da última resposta aceita; unknown não afirma atualização.
# TYPE nuvemcash_agent_version_status gauge
nuvemcash_agent_version_status{status=%q,installed_version=%q,latest_version=%q} 1
`, status, installed, latest)
	return err
}
