//go:build e2e

package main

import (
	"context"
	"log/slog"

	"github.com/nuvemcash/agent/wire"
)

// chartVerifier do build e2e aceita tudo: o e2e local publica charts sem assinatura num
// registry próprio. O release nunca compila com a tag e2e (Dockerfile, BUILD_TAGS).
func chartVerifier(string, bool) func(context.Context, wire.AgentUpdateTarget) error {
	return func(_ context.Context, t wire.AgentUpdateTarget) error {
		slog.Warn("origin verification skipped: e2e build", "version", t.Version)
		return nil
	}
}
