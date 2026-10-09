// Package update é o updater do agente: a atualização automática (ADR 0017). A cada
// execução ele pergunta à api qual é a versão alvo do cluster e aplica o chart inteiro
// daquela versão, fixado por digest, com a identidade própria do updater — cujo teto de
// permissões é o limite do que uma atualização consegue conceder. Pedir além do teto é
// recusado pelo próprio apiserver; o Helm reverte e o desfecho vai para a api.
package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/release"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/nuvemcash/agent/wire"
)

// errPendingMsg é o errPending (não exportado) do pacote action do Helm.
const errPendingMsg = "another operation (install/upgrade/rollback) is in progress"

// maxHistory espelha o default da CLI do Helm: sem ele o SDK guarda revisões sem limite.
const maxHistory = 10

// Updater executa UMA rodada de atualização automática da release do agente.
type Updater struct {
	API  API
	Helm *action.Configuration
	// Kube lê os recursos da release (só leitura) para decidir a abstenção.
	Kube      kubernetes.Interface
	Release   string
	Namespace string
	// Fetch baixa o chart do alvo pelo digest (OCIFetcher em produção).
	Fetch func(context.Context, wire.AgentUpdateTarget) (*chartv2.Chart, error)
	// Timeout limita a espera de readiness do upgrade e, de novo, a do rollback.
	Timeout time.Duration
	// PendingLimit é a duração máxima de uma execução: uma release em pending-* há mais
	// tempo que isso ficou órfã (o Job morreu no meio) e volta à última revisão deployed.
	PendingLimit time.Duration
}

// Run começa pela abstenção (abstain.go): release de GitOps ou de registry espelhado não é
// tocada. Devolve erro só quando a rodada não conseguiu chegar a um desfecho (api fora,
// chart inacessível, Helm sem acesso); upgrade revertido ou recusado pelo teto é desfecho
// relatado, não erro.
func (u *Updater) Run(ctx context.Context) error {
	// Manager do helm CLI: compartilha ownership com os applies manuais, nos dois sentidos (api#316).
	kube.ManagedFieldsManager = "helm"
	reason, version, err := u.abstention(ctx)
	if err != nil {
		return err
	}
	if reason != "" {
		return u.reportAbstention(ctx, reason, version)
	}
	installed, ok, err := u.installedVersion()
	if err != nil || !ok {
		return err
	}
	target, found, err := u.API.Target(ctx, installed)
	if err != nil {
		return err
	}
	if !found {
		slog.Info("agent up to date", "version", installed)
		return nil
	}
	ch, err := u.Fetch(ctx, target)
	if err != nil {
		return fmt.Errorf("fetch chart %s: %w", target.Version, err)
	}

	slog.Info("applying agent update", "from", installed, "to", target.Version, "digest", target.ChartDigest)
	up := action.NewUpgrade(u.Helm)
	up.Namespace = u.Namespace
	up.ResetThenReuseValues = true // values do usuário (token incluído) sobre os defaults novos
	up.RollbackOnFailure = true
	up.WaitStrategy = kube.StatusWatcherStrategy
	up.Timeout = u.Timeout
	up.MaxHistory = maxHistory
	// ServerSideApply fica em "auto": segue o método da release, e a instalada pelo Helm 3
	// continua em client-side apply.
	_, err = up.RunWithContext(ctx, u.Release, ch, map[string]any{})
	if err != nil && strings.Contains(err.Error(), errPendingMsg) {
		// Outra operação (um helm upgrade manual) começou no meio: não é falha da versão, e
		// relatá-la tiraria a versão das ofertas a este cluster. A próxima hora tenta de novo.
		slog.Info("release operation in progress, skipping", "err", err)
		return nil
	}

	report := wire.AgentUpdateReport{Version: target.Version, Outcome: wire.OutcomeApplied}
	switch {
	case err == nil:
	case beyondCeiling(err):
		report.Outcome, report.Reason = wire.OutcomeRejectedByCeiling, err.Error()
	default:
		report.Outcome, report.Reason = wire.OutcomeRolledBack, err.Error()
	}
	slog.Info("agent update outcome", "version", report.Version, "outcome", report.Outcome, "reason", report.Reason)
	return u.API.Report(ctx, report)
}

// installedVersion lê a versão do chart da última revisão deployed. Uma release presa em
// pending-* além do PendingLimit é revertida antes; dentro do limite, outra operação está
// em andamento e a rodada não faz nada (ok=false).
func (u *Updater) installedVersion() (string, bool, error) {
	last, err := asV1(u.Helm.Releases.Last(u.Release))
	if err != nil {
		return "", false, fmt.Errorf("read release %s: %w", u.Release, err)
	}
	if last.Info.Status.IsPending() {
		if age := time.Since(last.Info.LastDeployed); age < u.PendingLimit {
			slog.Info("release operation in progress, skipping", "status", last.Info.Status, "age", age)
			return "", false, nil
		}
		if err := u.recoverPending(last); err != nil {
			return "", false, err
		}
	}
	dep, err := asV1(u.Helm.Releases.Deployed(u.Release))
	if err != nil {
		return "", false, fmt.Errorf("read deployed release %s: %w", u.Release, err)
	}
	return dep.Chart.Metadata.Version, true, nil
}

func (u *Updater) recoverPending(stuck *rspb.Release) error {
	dep, err := asV1(u.Helm.Releases.Deployed(u.Release))
	if err != nil {
		return fmt.Errorf("release stuck in %s without deployed revision: %w", stuck.Info.Status, err)
	}
	slog.Warn("release stuck, rolling back to last deployed revision",
		"status", stuck.Info.Status, "revision", stuck.Version, "to", dep.Version)
	rb := action.NewRollback(u.Helm)
	rb.Version = dep.Version
	rb.WaitStrategy = kube.StatusWatcherStrategy
	rb.Timeout = u.Timeout
	rb.MaxHistory = maxHistory
	if err := rb.Run(u.Release); err != nil {
		return fmt.Errorf("rollback stuck release: %w", err)
	}
	return nil
}

// beyondCeiling reconhece a recusa do RBAC: o apiserver não deixa gravar uma role com
// permissão que o updater não tem (escalada), nem criar o que o teto não cobre.
func beyondCeiling(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "attempting to grant RBAC permissions not currently held") ||
		(apierrors.IsForbidden(err) && strings.Contains(msg, " cannot "))
}

func asV1(r release.Releaser, err error) (*rspb.Release, error) {
	if err != nil {
		return nil, err
	}
	rel, ok := r.(*rspb.Release)
	if !ok {
		return nil, fmt.Errorf("unsupported release type %T", r)
	}
	return rel, nil
}

// API fala com a api do nuvem.cash no contrato wire.AgentUpdate*.
type API struct {
	URL    string // base do ingest, a mesma do agente
	Token  string // token do cluster (nunca logar)
	Client *http.Client
}

// Target devolve a versão alvo do cluster; found=false quando não há nada a fazer.
func (a API) Target(ctx context.Context, installed string) (wire.AgentUpdateTarget, bool, error) {
	var t wire.AgentUpdateTarget
	u := strings.TrimRight(a.URL, "/") + wire.AgentUpdateTargetPath + "?agentVersion=" + url.QueryEscape(installed)
	resp, err := a.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return t, false, err
	}
	defer resp.Body.Close() //nolint:errcheck // corpo já lido; erro de close não é acionável
	switch resp.StatusCode {
	case http.StatusNoContent:
		return t, false, nil
	case http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&t); err != nil {
			return t, false, fmt.Errorf("decode target: %w", err)
		}
		if t.Version == "" || !strings.HasPrefix(t.ChartDigest, "sha256:") {
			return t, false, fmt.Errorf("invalid target %+v", t)
		}
		return t, true, nil
	default:
		return t, false, fmt.Errorf("target: unexpected status %d", resp.StatusCode)
	}
}

// Report relata o desfecho de uma tentativa.
func (a API) Report(ctx context.Context, r wire.AgentUpdateReport) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	resp, err := a.do(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+wire.AgentUpdateOutcomePath, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // nada a ler além do status
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("report outcome: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (a API) do(ctx context.Context, method, u string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent update api: %w", err)
	}
	return resp, nil
}
