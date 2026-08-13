package collect

import (
	"os"
	"testing"
)

func TestParseSummary(t *testing.T) {
	raw, err := os.ReadFile("testdata/summary.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	samples, err := parseSummary(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Pod sem stats de cpu/memória (nil) é pulado sem pânico.
	if len(samples) != 1 {
		t.Fatalf("esperava 1 amostra, veio %d", len(samples))
	}
	s := samples[0]
	if s.Namespace != "app" || s.PodName != "web-abc" || s.PodUID != "u1" {
		t.Fatalf("identidade errada: %+v", s)
	}
	if s.CPUUsageCoreSeconds != 7200.0 { // 7_200_000_000_000 ns = 7200 core·s
		t.Fatalf("cpu cumulativo errado: %v", s.CPUUsageCoreSeconds)
	}
	if s.WorkingSetBytes != 104857600 {
		t.Fatalf("working set errado: %v", s.WorkingSetBytes)
	}
}
