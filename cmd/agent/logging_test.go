package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLogHandlerJSONEmitsOneObjectPerRecord(t *testing.T) {
	var buf bytes.Buffer
	slog.New(newLogHandler(&buf, "json", slog.LevelInfo)).Info("agent started", "version", "v1.2.3")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("saída não é um objeto JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "agent started" || rec["version"] != "v1.2.3" || rec["level"] != "INFO" {
		t.Fatalf("registro JSON inesperado: %v", rec)
	}
}

func TestLogHandlerTextIsNotJSON(t *testing.T) {
	var buf bytes.Buffer
	slog.New(newLogHandler(&buf, "text", slog.LevelInfo)).Info("agent started")

	out := buf.String()
	if json.Valid([]byte(strings.TrimSpace(out))) || !strings.Contains(out, `msg="agent started"`) {
		t.Fatalf("formato text esperado, veio: %s", out)
	}
}

func TestLogHandlerHonorsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newLogHandler(&buf, "json", slog.LevelWarn))
	logger.Info("ignored")
	logger.Warn("kept")

	out := buf.String()
	if strings.Contains(out, "ignored") || !strings.Contains(out, "kept") {
		t.Fatalf("nível warn deveria filtrar info: %s", out)
	}
}
