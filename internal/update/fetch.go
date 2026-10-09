package update

import (
	"bytes"
	"context"
	"fmt"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/registry"

	"github.com/nuvemcash/agent/wire"
)

// OCIFetcher baixa o chart do alvo do registry OCI pelo DIGEST, nunca pela tag: uma tag
// movida no registry não chega ao cluster.
type OCIFetcher struct {
	Chart     string // oci://ghcr.io/nuvemcash/charts/nuvemcash-agent
	PlainHTTP bool
}

func (f OCIFetcher) Fetch(_ context.Context, t wire.AgentUpdateTarget) (*chartv2.Chart, error) {
	var opts []registry.ClientOption
	if f.PlainHTTP {
		opts = append(opts, registry.ClientOptPlainHTTP())
	}
	c, err := registry.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	// Sem tag na referência: com "repo:tag@digest" o cliente do Helm puxaria pela tag.
	res, err := c.Pull(f.Chart + "@" + t.ChartDigest)
	if err != nil {
		return nil, err
	}
	if res.Manifest.Digest != t.ChartDigest {
		return nil, fmt.Errorf("chart digest mismatch: got %s, want %s", res.Manifest.Digest, t.ChartDigest)
	}
	ch, err := loader.LoadArchive(bytes.NewReader(res.Chart.Data))
	if err != nil {
		return nil, err
	}
	if ch.Metadata.Version != t.Version {
		return nil, fmt.Errorf("chart %s has version %s, want %s", t.ChartDigest, ch.Metadata.Version, t.Version)
	}
	return ch, nil
}
