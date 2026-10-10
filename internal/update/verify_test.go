package update_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nuvemcash/agent/internal/update"
	"github.com/nuvemcash/agent/wire"
)

// Chart 0.6.4 publicado pelo release.yml; o bundle em testdata é o que o cosign anexou a
// ele no GHCR (DSSE in-toto, Rekor com prova e SET, TSA).
const chart064Digest = "sha256:bdc398c25a38d90eb271434f390093103fd54c82ffb06a134c4ad2875fbbcd2e"

const bundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// fakeRegistry serve manifestos e blobs como o GHCR: o índice na tag de fallback
// sha256-<hex> e um manifesto por bundle, com o bundle na única camada.
type fakeRegistry struct {
	objects map[string][]byte // "manifests/<ref>" ou "blobs/<digest>"
	status  int               // != 0: toda requisição responde com este status
}

func digestOf(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// addBundle publica um manifesto de bundle no formato do cosign v3 e devolve o descritor
// para o índice. O artifactType do índice sai "empty", como o GHCR grava.
func (r *fakeRegistry) addBundle(t *testing.T, subject string, bundle []byte) map[string]any {
	t.Helper()
	r.objects["blobs/"+digestOf(bundle)] = bundle
	m := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"artifactType":  bundleMediaType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.empty.v1+json", "size": 2,
			"digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		},
		"layers": []any{map[string]any{
			"mediaType": bundleMediaType, "size": len(bundle), "digest": digestOf(bundle),
		}},
		"subject": map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "size": 704, "digest": subject,
		},
	})
	r.objects["manifests/"+digestOf(m)] = m
	return map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json", "size": len(m),
		"digest": digestOf(m), "artifactType": "application/vnd.oci.empty.v1+json",
	}
}

func (r *fakeRegistry) setIndex(t *testing.T, chartDigest string, manifests ...map[string]any) {
	t.Helper()
	r.objects["manifests/"+strings.Replace(chartDigest, ":", "-", 1)] = mustJSON(t, map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": manifests,
	})
}

func (r *fakeRegistry) serve(t *testing.T) string {
	t.Helper()
	const prefix = "/v2/nuvemcash/charts/nuvemcash-agent/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.status != 0 {
			w.WriteHeader(r.status)
			return
		}
		key := strings.TrimPrefix(req.URL.Path, prefix)
		b, ok := r.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ct := "application/octet-stream"
		if strings.HasPrefix(key, "manifests/") {
			var m struct{ MediaType string }
			_ = json.Unmarshal(b, &m)
			ct = m.MediaType
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Docker-Content-Digest", digestOf(b))
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return "oci://" + strings.TrimPrefix(srv.URL, "http://") + "/nuvemcash/charts/nuvemcash-agent"
}

func realBundle(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/bundle-0.6.4.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tamperedBundle troca a assinatura do envelope DSSE: o JSON segue válido, a criptografia não.
func tamperedBundle(t *testing.T) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(realBundle(t), &b); err != nil {
		t.Fatal(err)
	}
	sig := b["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)
	sig["sig"] = "MEUCIQCtamperedtamperedtamperedtamperedtamperedtamperedAiBtamperedtamperedtamperedtampered"
	return mustJSON(t, b)
}

func newRegistry() *fakeRegistry { return &fakeRegistry{objects: map[string][]byte{}} }

func verifyTarget(t *testing.T, reg *fakeRegistry, version, digest string) error {
	t.Helper()
	v := update.OriginVerifier{Chart: reg.serve(t), PlainHTTP: true}
	return v.Verify(context.Background(), wire.AgentUpdateTarget{Version: version, ChartDigest: digest})
}

func TestOriginVerifierAcceptsReleaseSignature(t *testing.T) {
	reg := newRegistry()
	reg.setIndex(t, chart064Digest, reg.addBundle(t, chart064Digest, realBundle(t)))
	if err := verifyTarget(t, reg, "0.6.4", chart064Digest); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestOriginVerifierAcceptsOneValidAmongInvalid(t *testing.T) {
	reg := newRegistry()
	reg.setIndex(t, chart064Digest,
		reg.addBundle(t, chart064Digest, tamperedBundle(t)),
		reg.addBundle(t, chart064Digest, realBundle(t)))
	if err := verifyTarget(t, reg, "0.6.4", chart064Digest); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestOriginVerifierRejects(t *testing.T) {
	otherDigest := digestOf([]byte("outro chart"))
	cases := map[string]struct {
		setup   func(t *testing.T, reg *fakeRegistry)
		version string
		digest  string
	}{
		// Assinatura válida de outra versão não serve de passe: a SAN amarra a tag.
		"other version": {
			setup: func(t *testing.T, reg *fakeRegistry) {
				reg.setIndex(t, chart064Digest, reg.addBundle(t, chart064Digest, realBundle(t)))
			},
			version: "0.6.5", digest: chart064Digest,
		},
		// Bundle verdadeiro copiado para a tag de outro digest: o subject in-toto não bate.
		"swapped digest": {
			setup: func(t *testing.T, reg *fakeRegistry) {
				reg.setIndex(t, otherDigest, reg.addBundle(t, otherDigest, realBundle(t)))
			},
			version: "0.6.4", digest: otherDigest,
		},
		"no valid bundle": {
			setup: func(t *testing.T, reg *fakeRegistry) {
				reg.setIndex(t, chart064Digest, reg.addBundle(t, chart064Digest, tamperedBundle(t)))
			},
			version: "0.6.4", digest: chart064Digest,
		},
		"empty index": {
			setup:   func(t *testing.T, reg *fakeRegistry) { reg.setIndex(t, chart064Digest) },
			version: "0.6.4", digest: chart064Digest,
		},
		// Índice adulterado com referência inválida não pode virar "transitório" eterno.
		"malformed manifest digest": {
			setup: func(t *testing.T, reg *fakeRegistry) {
				reg.setIndex(t, chart064Digest, map[string]any{"digest": "sha256:zz"})
			},
			version: "0.6.4", digest: chart064Digest,
		},
		// Chart sem assinatura alguma: a tag de fallback não existe.
		"unsigned": {
			setup:   func(*testing.T, *fakeRegistry) {},
			version: "0.6.4", digest: chart064Digest,
		},
		"bundle blob missing": {
			setup: func(t *testing.T, reg *fakeRegistry) {
				reg.setIndex(t, chart064Digest, reg.addBundle(t, chart064Digest, realBundle(t)))
				delete(reg.objects, "blobs/"+digestOf(realBundle(t)))
			},
			version: "0.6.4", digest: chart064Digest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reg := newRegistry()
			tc.setup(t, reg)
			err := verifyTarget(t, reg, tc.version, tc.digest)
			if !errors.Is(err, update.ErrSignatureInvalid) {
				t.Fatalf("Verify = %v, want ErrSignatureInvalid", err)
			}
		})
	}
}

func TestOriginVerifierTransientErrors(t *testing.T) {
	t.Run("5xx", func(t *testing.T) {
		reg := newRegistry()
		reg.setIndex(t, chart064Digest, reg.addBundle(t, chart064Digest, realBundle(t)))
		reg.status = http.StatusServiceUnavailable
		err := verifyTarget(t, reg, "0.6.4", chart064Digest)
		if err == nil || errors.Is(err, update.ErrSignatureInvalid) {
			t.Fatalf("Verify = %v, want transient error", err)
		}
	})
	t.Run("network", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		host := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		v := update.OriginVerifier{Chart: "oci://" + host + "/nuvemcash/charts/nuvemcash-agent", PlainHTTP: true}
		err := v.Verify(context.Background(), wire.AgentUpdateTarget{Version: "0.6.4", ChartDigest: chart064Digest})
		if err == nil || errors.Is(err, update.ErrSignatureInvalid) {
			t.Fatalf("Verify = %v, want transient error", err)
		}
	})
}
