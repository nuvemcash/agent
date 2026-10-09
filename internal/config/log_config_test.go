package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestLoad_LogDefaults(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	c, err := Load()
	if err != nil {
		t.Fatalf("esperava sucesso, veio %v", err)
	}
	if c.LogFormat != "json" || c.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults de log errados: format=%q level=%v", c.LogFormat, c.LogLevel)
	}
}

func TestLoad_LogOverrides(t *testing.T) {
	t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
	t.Setenv("NUVEMCASH_AGENT_LOG_FORMAT", "text")
	t.Setenv("NUVEMCASH_AGENT_LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil || c.LogFormat != "text" || c.LogLevel != slog.LevelDebug {
		t.Fatalf("override de log falhou: %+v, %v", c, err)
	}
}

// Valor inválido de log falha na partida e nomeia a variável, em vez de cair num default
// silencioso.
func TestLoad_InvalidLogSettingsFailFast(t *testing.T) {
	cases := []struct{ key, value string }{
		{"NUVEMCASH_AGENT_LOG_FORMAT", "yaml"},
		{"NUVEMCASH_AGENT_LOG_FORMAT", "JSON"},
		{"NUVEMCASH_AGENT_LOG_LEVEL", "trace"},
		{"NUVEMCASH_AGENT_LOG_LEVEL", "INFO"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv("NUVEMCASH_AGENT_TOKEN", "tok")
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("esperava erro citando %s, veio %v", tc.key, err)
			}
		})
	}
}
