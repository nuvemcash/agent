// Package config carrega a configuração do agente por env vars NUVEMCASH_AGENT_*.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Token          string        // token de conexão do cluster (nunca logar)
	URL            string        // base do ingest (default: https://ingest.nuvem.cash)
	ScrapeInterval time.Duration // kubelet Summary por nó
	ShipInterval   time.Duration // fechamento/envio da janela
	BufferWindows  int           // janelas retidas em memória quando o envio falha
}

func Load() (Config, error) {
	c := Config{
		Token:          os.Getenv("NUVEMCASH_AGENT_TOKEN"),
		URL:            getenvDefault("NUVEMCASH_AGENT_URL", "https://ingest.nuvem.cash"),
		ScrapeInterval: 60 * time.Second,
		ShipInterval:   5 * time.Minute,
		BufferWindows:  144,
	}
	if c.Token == "" {
		return Config{}, errors.New("NUVEMCASH_AGENT_TOKEN é obrigatório")
	}
	var err error
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
