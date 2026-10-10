//go:build !e2e

package main

import (
	"context"

	"github.com/nuvemcash/agent/internal/update"
	"github.com/nuvemcash/agent/wire"
)

// chartVerifier é a Verificação de origem (ADR 0020). Não há como desligá-la no binário
// publicado; só o build do e2e local (tag e2e) a troca.
func chartVerifier(chart string, plainHTTP bool) func(context.Context, wire.AgentUpdateTarget) error {
	return update.OriginVerifier{Chart: chart, PlainHTTP: plainHTTP}.Verify
}
