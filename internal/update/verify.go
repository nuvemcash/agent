package update

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/nuvemcash/agent/wire"
)

// trustedRootJSON é o material de confiança do Sigstore público (Fulcio, Rekor, CT, TSA),
// versionado e embutido: o cluster do cliente não fala com TUF nem com Rekor. O
// TestTrustedRootMatchesTUF falha o release quando ele diverge do publicado pelo TUF.
//
//go:embed trusted_root.json
var trustedRootJSON []byte

const (
	bundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	signerIssuer    = "https://token.actions.githubusercontent.com"
	signerWorkflow  = "https://github.com/nuvemcash/agent/.github/workflows/release.yml@refs/tags/v"
)

// ErrSignatureInvalid marca a recusa da Verificação de origem: o chart não tem assinatura
// do release.yml daquela versão. Qualquer outro erro do Verify é transitório (rede, 5xx).
var ErrSignatureInvalid = errors.New("chart signature invalid")

// OriginVerifier confere que o chart alvo foi assinado (cosign keyless) pelo release.yml de
// nuvemcash/agent na tag v<versão>. O bundle vem do próprio registry do chart, pela tag de
// fallback do OCI 1.1 (sha256-<hex>): o GHCR não implementa a referrers API.
type OriginVerifier struct {
	Chart     string // oci://ghcr.io/nuvemcash/charts/nuvemcash-agent
	PlainHTTP bool
}

// Verify aceita se ao menos um bundle do índice passar: um re-sign ou um artefato extra no
// índice não quebra a verificação. Erro transitório em algum bundle só vira o resultado
// quando nenhum passou.
func (v OriginVerifier) Verify(ctx context.Context, t wire.AgentUpdateTarget) error {
	hexDigest, ok := strings.CutPrefix(t.ChartDigest, "sha256:")
	artifact, err := hex.DecodeString(hexDigest)
	if !ok || err != nil || len(artifact) != 32 {
		return fmt.Errorf("%w: malformed chart digest %q", ErrSignatureInvalid, t.ChartDigest)
	}
	verifier, policy, err := newPolicy(artifact, t.Version)
	if err != nil {
		return err
	}
	repo, err := remote.NewRepository(strings.TrimPrefix(v.Chart, "oci://"))
	if err != nil {
		return err
	}
	repo.PlainHTTP = v.PlainHTTP
	// Sem retry: a próxima execução (1h) já é a nova tentativa.
	repo.Client = &auth.Client{Cache: auth.NewCache()}

	var index struct {
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	if err := fetchJSON(ctx, repo, "sha256-"+hexDigest, &index); err != nil {
		return err
	}
	var transient, lastInvalid error
	for _, m := range index.Manifests {
		err := verifyManifest(ctx, repo, m.Digest, verifier, policy)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrSignatureInvalid):
			lastInvalid = err
		default:
			transient = err
		}
	}
	if transient != nil {
		return transient
	}
	if lastInvalid == nil {
		return fmt.Errorf("%w: no bundle attached to %s", ErrSignatureInvalid, t.ChartDigest)
	}
	return lastInvalid
}

func verifyManifest(ctx context.Context, repo *remote.Repository, digest string,
	verifier *verify.Verifier, policy verify.PolicyBuilder) error {
	var manifest struct {
		Layers []struct{ MediaType, Digest string } `json:"layers"`
	}
	if err := fetchJSON(ctx, repo, digest, &manifest); err != nil {
		return err
	}
	err := fmt.Errorf("%w: manifest %s carries no sigstore bundle", ErrSignatureInvalid, digest)
	for _, l := range manifest.Layers {
		if l.MediaType != bundleMediaType {
			continue
		}
		_, raw, ferr := oras.FetchBytes(ctx, repo.Blobs(), l.Digest, oras.DefaultFetchBytesOptions)
		if ferr != nil {
			if err = classify(ferr); !errors.Is(err, ErrSignatureInvalid) {
				return err
			}
			continue
		}
		var b bundle.Bundle
		if err = b.UnmarshalJSON(raw); err != nil {
			err = fmt.Errorf("%w: bundle %s: %v", ErrSignatureInvalid, l.Digest, err)
			continue
		}
		if _, err = verifier.Verify(&b, policy); err != nil {
			err = fmt.Errorf("%w: bundle %s: %v", ErrSignatureInvalid, l.Digest, err)
			continue
		}
		return nil
	}
	return err
}

// newPolicy: SAN exata do release.yml na tag da versão alvo, emissor do GitHub Actions,
// artefato = digest do chart, e exige Rekor (prova de inclusão), TSA e SCT do Fulcio.
func newPolicy(artifact []byte, version string) (*verify.Verifier, verify.PolicyBuilder, error) {
	trusted, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return nil, verify.PolicyBuilder{}, fmt.Errorf("embedded trusted root: %w", err)
	}
	verifier, err := verify.NewVerifier(trusted,
		verify.WithTransparencyLog(1), verify.WithSignedTimestamps(1), verify.WithSignedCertificateTimestamps(1))
	if err != nil {
		return nil, verify.PolicyBuilder{}, err
	}
	id, err := verify.NewShortCertificateIdentity(signerIssuer, "", signerWorkflow+version, "")
	if err != nil {
		return nil, verify.PolicyBuilder{}, err
	}
	return verifier, verify.NewPolicy(verify.WithArtifactDigest("sha256", artifact), verify.WithCertificateIdentity(id)), nil
}

func fetchJSON(ctx context.Context, repo *remote.Repository, ref string, v any) error {
	_, raw, err := oras.FetchBytes(ctx, repo, ref, oras.DefaultFetchBytesOptions)
	if err != nil {
		return classify(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrSignatureInvalid, ref, err)
	}
	return nil
}

// classify: o que o registry respondeu de fato (ausente, referência inválida no índice,
// conteúdo que não bate com o digest, grande demais) é assinatura inválida; o resto (rede,
// 5xx, 429, auth) é transitório.
func classify(err error) error {
	if errors.Is(err, errdef.ErrNotFound) || errors.Is(err, errdef.ErrSizeExceedsLimit) ||
		errors.Is(err, errdef.ErrInvalidReference) || errors.Is(err, errdef.ErrInvalidDigest) ||
		errors.Is(err, content.ErrMismatchedDigest) || errors.Is(err, content.ErrTrailingData) {
		return fmt.Errorf("%w: %v", ErrSignatureInvalid, err)
	}
	return err
}
