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

func TestMetricsDefaultAndOptOut(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	t.Setenv("NUVEMCASH_AGENT_METRICS_ENABLED", "")
	cfg, err := Load()
	if err != nil || !cfg.MetricsEnabled {
		t.Fatalf("métricas devem vir habilitadas: %+v, %v", cfg, err)
	}
	t.Setenv("NUVEMCASH_AGENT_METRICS_ENABLED", "false")
	cfg, err = Load()
	if err != nil || cfg.MetricsEnabled {
		t.Fatalf("opt-out deve ser respeitado: %+v, %v", cfg, err)
	}
	t.Setenv("NUVEMCASH_AGENT_METRICS_ENABLED", "talvez")
	if _, err := Load(); err == nil {
		t.Fatal("booleano inválido deve ser recusado")
	}
}

func TestLoadUpdaterDefaultsAndRequired(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	t.Setenv("NUVEMCASH_UPDATER_RELEASE", "nuvemcash-agent")
	t.Setenv("NUVEMCASH_UPDATER_NAMESPACE", "nuvemcash-system")
	c, err := LoadUpdater()
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://ingest.nuvem.cash" || c.Chart != "oci://ghcr.io/nuvemcash/charts/nuvemcash-agent" ||
		c.Timeout != 5*time.Minute || c.PlainHTTP || c.Release != "nuvemcash-agent" || c.Namespace != "nuvemcash-system" {
		t.Fatalf("defaults errados: %+v", c)
	}
	for _, k := range []string{"NUVEMCASH_AGENT_TOKEN", "NUVEMCASH_UPDATER_RELEASE", "NUVEMCASH_UPDATER_NAMESPACE"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "")
			if _, err := LoadUpdater(); err == nil {
				t.Fatalf("%s vazio devia falhar", k)
			}
		})
	}
}

func TestLoadUpdaterOverrides(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	t.Setenv("NUVEMCASH_UPDATER_RELEASE", "r")
	t.Setenv("NUVEMCASH_UPDATER_NAMESPACE", "ns")
	t.Setenv("NUVEMCASH_UPDATER_CHART", "oci://registry:5000/charts/nuvemcash-agent")
	t.Setenv("NUVEMCASH_UPDATER_PLAIN_HTTP", "true")
	t.Setenv("NUVEMCASH_UPDATER_TIMEOUT", "45s")
	c, err := LoadUpdater()
	if err != nil || c.Chart != "oci://registry:5000/charts/nuvemcash-agent" || !c.PlainHTTP || c.Timeout != 45*time.Second {
		t.Fatalf("override falhou: %+v %v", c, err)
	}
	t.Setenv("NUVEMCASH_UPDATER_PLAIN_HTTP", "talvez")
	if _, err := LoadUpdater(); err == nil {
		t.Fatal("booleano inválido devia falhar")
	}
}

func TestAutoUpgradeDefaultAndOptOut(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	t.Setenv("NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED", "")
	cfg, err := Load()
	if err != nil || !cfg.AutoUpgradeEnabled {
		t.Fatalf("sem a env (chart antigo), a atualização automática vale ligada: %+v, %v", cfg, err)
	}
	t.Setenv("NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED", "false")
	cfg, err = Load()
	if err != nil || cfg.AutoUpgradeEnabled {
		t.Fatalf("desligada deve ser respeitada: %+v, %v", cfg, err)
	}
	t.Setenv("NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED", "talvez")
	if _, err := Load(); err == nil {
		t.Fatal("booleano inválido deve ser recusado")
	}
}
