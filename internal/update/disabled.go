package update

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/nuvemcash/agent/wire"
)

// maxDisabledRetry é o teto do backoff do relato de desligamento.
const maxDisabledRetry = time.Hour

// ReportDisabled relata à api que a atualização automática está desligada
// (autoUpgrade.enabled=false). Sem o updater não há CronJob, então é o coletor quem relata,
// uma vez por partida; sem isso a api seguiria mostrando o último desfecho do updater como
// se ele estivesse ativo. Tenta de novo, com backoff dobrando a partir de retry, até a api
// aceitar ou ctx acabar. Build sem versão de release ("dev") não relata: a api recusaria.
func ReportDisabled(ctx context.Context, a API, version string, retry time.Duration) error {
	if !semver.IsValid("v" + strings.TrimPrefix(version, "v")) {
		slog.Info("auto upgrade disabled, not reported: not a release version", "version", version)
		return nil
	}
	report := wire.AgentUpdateReport{Version: strings.TrimPrefix(version, "v"),
		Outcome: wire.OutcomeAbstained, Reason: wire.ReasonDisabled}
	for {
		err := a.Report(ctx, report)
		if err == nil {
			slog.Info("auto upgrade disabled, reported", "version", report.Version)
			return nil
		}
		slog.Warn("report auto upgrade disabled failed, retrying", "in", retry, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
		retry = min(retry*2, maxDisabledRetry)
	}
}
