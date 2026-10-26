package agent

import (
	"os"
	"testing"

	"golang.org/x/mod/modfile"
)

// O wire é módulo próprio e só da stdlib: consumidores (a api) não podem herdar o grafo
// k8s.io do agente via MVS (nuvemcash/agent#1).
func TestWireIsStdlibOnlyModule(t *testing.T) {
	data, err := os.ReadFile("wire/go.mod")
	if err != nil {
		t.Fatalf("wire/go.mod: %v", err)
	}
	f, err := modfile.Parse("wire/go.mod", data, nil)
	if err != nil {
		t.Fatalf("parse wire/go.mod: %v", err)
	}
	if got, want := f.Module.Mod.Path, "github.com/nuvemcash/agent/wire"; got != want {
		t.Errorf("module = %q, want %q", got, want)
	}
	if len(f.Require) != 0 {
		t.Errorf("wire/go.mod tem %d require; o contrato de fio deve depender só da stdlib", len(f.Require))
	}
}
