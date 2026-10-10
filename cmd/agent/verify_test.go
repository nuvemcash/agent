//go:build !e2e

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nuvemcash/agent/internal/update"
	"github.com/nuvemcash/agent/wire"
)

// O binário publicado verifica a origem: um chart sem assinatura no registry é recusado.
// Guarda contra o verificador permissivo do build e2e vazar para o build normal.
func TestChartVerifierRejectsUnsignedChart(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	chart := "oci://" + strings.TrimPrefix(srv.URL, "http://") + "/charts/nuvemcash-agent"

	err := chartVerifier(chart, true)(context.Background(), wire.AgentUpdateTarget{
		Version:     "0.6.4",
		ChartDigest: "sha256:bdc398c25a38d90eb271434f390093103fd54c82ffb06a134c4ad2875fbbcd2e",
	})
	if !errors.Is(err, update.ErrSignatureInvalid) {
		t.Fatalf("err = %v, quer ErrSignatureInvalid", err)
	}
}
