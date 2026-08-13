package config

import (
	"testing"
	"time"
)

func TestLoad_DefaultsEObrigatorios(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	c, err := Load()
	if err != nil {
		t.Fatalf("esperava sucesso, veio %v", err)
	}
	if c.URL != "https://ingest.nuvem.cash" || c.ScrapeInterval != 60*time.Second ||
		c.ShipInterval != 5*time.Minute || c.BufferWindows != 144 {
		t.Fatalf("defaults errados: %+v", c)
	}

	t.Setenv("NUVEMCASH_AGENT_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("token vazio devia falhar")
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	t.Setenv("NUVEMCASH_AGENT_URL", "http://devsink:8081")
	t.Setenv("NUVEMCASH_AGENT_SCRAPE_INTERVAL", "30s")
	c, err := Load()
	if err != nil || c.URL != "http://devsink:8081" || c.ScrapeInterval != 30*time.Second {
		t.Fatalf("override falhou: %+v %v", c, err)
	}
}
