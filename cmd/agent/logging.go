package main

import (
	"io"
	"log/slog"
)

// newLogHandler monta o handler do processo. JSON é o padrão para pipelines de log; text
// fica para quem lê direto no kubectl logs. O valor já foi validado em config.Load.
func newLogHandler(w io.Writer, format string, level slog.Level) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}
