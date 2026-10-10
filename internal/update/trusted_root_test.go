package update

import (
	"bytes"
	"os"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/tuf"
)

// TestTrustedRootMatchesTUF falha quando o trusted_root.json embutido diverge do publicado
// pelo TUF do Sigstore: uma rotação vira diff revisado antes de chegar ao parque. Exige
// rede, então só roda com SIGSTORE_TUF_CHECK=1 (o CI e o release ligam).
func TestTrustedRootMatchesTUF(t *testing.T) {
	if os.Getenv("SIGSTORE_TUF_CHECK") != "1" {
		t.Skip("SIGSTORE_TUF_CHECK=1 para comparar com o TUF do Sigstore (rede)")
	}
	opts := tuf.DefaultOptions()
	opts.CachePath = t.TempDir()
	c, err := tuf.New(opts)
	if err != nil {
		t.Fatalf("tuf client: %v", err)
	}
	want, err := c.GetTarget("trusted_root.json")
	if err != nil {
		t.Fatalf("tuf trusted_root.json: %v", err)
	}
	if !bytes.Equal(trustedRootJSON, want) {
		t.Fatalf("internal/update/trusted_root.json diverge do TUF do Sigstore; atualize com o publicado:\n%s", want)
	}
}
