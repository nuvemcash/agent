// Package config carrega a configuração do agente por env vars NUVEMCASH_AGENT_*.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// logLevels aceita só os nomes minúsculos do chart; qualquer outro valor falha na partida.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

type Config struct {
	MetricsEnabled bool
	LogFormat      string        // "json" (default) ou "text"
	LogLevel       slog.Level    // debug, info (default), warn ou error
	Token          string        // token de conexão do cluster (nunca logar)
	URL            string        // base do ingest (default: https://ingest.nuvem.cash)
	ScrapeInterval time.Duration // kubelet Summary por nó
	ShipInterval   time.Duration // fechamento/envio da janela
	BufferWindows  int           // janelas retidas em memória quando o envio falha
	// BufferBytes limita a fila em BYTES COMPRIMIDOS. É o teto que protege de verdade: o de
	// janelas não sabe quão grande é o cluster, e num cluster grande as 144 janelas nominais
	// não cabiam nos 256Mi do container. Vale o que estourar primeiro.
	BufferBytes int
	// AutoUpgradeEnabled espelha autoUpgrade.enabled do chart. Desligada, não há updater
	// para relatar nada, e é o coletor que conta à api (update.ReportDisabled). Sem a env
	// (chart anterior à atualização automática) vale ligada: não relata nada.
	AutoUpgradeEnabled bool
}

func Load() (Config, error) {
	c := Config{
		MetricsEnabled:     true,
		AutoUpgradeEnabled: true,
		LogFormat:          getenvDefault("NUVEMCASH_AGENT_LOG_FORMAT", "json"),
		Token:              os.Getenv("NUVEMCASH_AGENT_TOKEN"),
		URL:                getenvDefault("NUVEMCASH_AGENT_URL", "https://ingest.nuvem.cash"),
		ScrapeInterval:     60 * time.Second,
		ShipInterval:       5 * time.Minute,
		BufferWindows:      144,
		// 32 MiB comprimidos: a ~250 KB por janela de cluster grande dá ~130 janelas (~11h a
		// cada 5 min), perto das 12h nominais, e sobra folga confortável dentro dos 256Mi
		// depois dos caches do informer.
		BufferBytes: 32 << 20,
	}
	if c.Token == "" {
		return Config{}, errors.New("NUVEMCASH_AGENT_TOKEN é obrigatório")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return Config{}, fmt.Errorf("NUVEMCASH_AGENT_LOG_FORMAT inválido: %q (use json ou text)", c.LogFormat)
	}
	levelName := getenvDefault("NUVEMCASH_AGENT_LOG_LEVEL", "info")
	level, ok := logLevels[levelName]
	if !ok {
		return Config{}, fmt.Errorf("NUVEMCASH_AGENT_LOG_LEVEL inválido: %q (use debug, info, warn ou error)", levelName)
	}
	c.LogLevel = level
	var err error
	if v := os.Getenv("NUVEMCASH_AGENT_METRICS_ENABLED"); v != "" {
		c.MetricsEnabled, err = strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("NUVEMCASH_AGENT_METRICS_ENABLED inválido: %q", v)
		}
	}
	if v := os.Getenv("NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED"); v != "" {
		if c.AutoUpgradeEnabled, err = strconv.ParseBool(v); err != nil {
			return Config{}, fmt.Errorf("NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED inválido: %q", v)
		}
	}
	if c.ScrapeInterval, err = durationDefault("NUVEMCASH_AGENT_SCRAPE_INTERVAL", c.ScrapeInterval); err != nil {
		return Config{}, err
	}
	if c.ShipInterval, err = durationDefault("NUVEMCASH_AGENT_SHIP_INTERVAL", c.ShipInterval); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("NUVEMCASH_AGENT_BUFFER_WINDOWS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("NUVEMCASH_AGENT_BUFFER_WINDOWS inválido: %q", v)
		}
		c.BufferWindows = n
	}
	if v := os.Getenv("NUVEMCASH_AGENT_BUFFER_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("NUVEMCASH_AGENT_BUFFER_BYTES inválido: %q", v)
		}
		c.BufferBytes = n
	}
	return c, nil
}

func getenvDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func durationDefault(k string, d time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return d, nil
	}
	out, err := time.ParseDuration(v)
	if err != nil || out <= 0 {
		return 0, fmt.Errorf("%s inválido: %q", k, v)
	}
	return out, nil
}

// Updater é a configuração do subcomando "update" (atualização automática). Token e URL
// são os mesmos do agente; o resto o chart injeta no CronJob.
type Updater struct {
	Token     string // token do cluster (nunca logar)
	URL       string
	Release   string // release Helm do agente
	Namespace string
	Chart     string        // repositório OCI do chart, sem tag
	PlainHTTP bool          // só para registry local de teste (e2e)
	Timeout   time.Duration // espera de readiness do upgrade e do rollback
}

func LoadUpdater() (Updater, error) {
	c := Updater{
		Token:     os.Getenv("NUVEMCASH_AGENT_TOKEN"),
		URL:       getenvDefault("NUVEMCASH_AGENT_URL", "https://ingest.nuvem.cash"),
		Release:   os.Getenv("NUVEMCASH_UPDATER_RELEASE"),
		Namespace: os.Getenv("NUVEMCASH_UPDATER_NAMESPACE"),
		Chart:     getenvDefault("NUVEMCASH_UPDATER_CHART", "oci://ghcr.io/nuvemcash/charts/nuvemcash-agent"),
	}
	for k, v := range map[string]string{"NUVEMCASH_AGENT_TOKEN": c.Token,
		"NUVEMCASH_UPDATER_RELEASE": c.Release, "NUVEMCASH_UPDATER_NAMESPACE": c.Namespace} {
		if v == "" {
			return Updater{}, fmt.Errorf("%s é obrigatório", k)
		}
	}
	var err error
	if v := os.Getenv("NUVEMCASH_UPDATER_PLAIN_HTTP"); v != "" {
		if c.PlainHTTP, err = strconv.ParseBool(v); err != nil {
			return Updater{}, fmt.Errorf("NUVEMCASH_UPDATER_PLAIN_HTTP inválido: %q", v)
		}
	}
	if c.Timeout, err = durationDefault("NUVEMCASH_UPDATER_TIMEOUT", 5*time.Minute); err != nil {
		return Updater{}, err
	}
	return c, nil
}
