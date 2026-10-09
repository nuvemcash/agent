package update_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nuvemcash/agent/internal/update"
	"github.com/nuvemcash/agent/wire"
)

const (
	releaseName = "nuvemcash-agent"
	namespace   = "nuvemcash-system"
	token       = "tok-123"
	chartDir    = "../../charts/nuvemcash-agent"
)

// fakeAPI simula o contrato da api (api#325): alvo por GET e relato por POST, com o
// Bearer do cluster.
type fakeAPI struct {
	mu        sync.Mutex
	target    *wire.AgentUpdateTarget
	queried   []string // agentVersion de cada consulta
	reports   []wire.AgentUpdateReport
	badTokens int
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			f.badTokens++
			return false
		}
		return true
	}
	mux.HandleFunc("GET "+wire.AgentUpdateTargetPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.queried = append(f.queried, r.URL.Query().Get("agentVersion"))
		if f.target == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(f.target)
	})
	mux.HandleFunc("POST "+wire.AgentUpdateOutcomePath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var rep wire.AgentUpdateReport
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.reports = append(f.reports, rep)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// failOnce falha só a PRIMEIRA aplicação no cluster — o rollback que vem depois passa,
// como no cluster de verdade.
type failOnce struct {
	*kubefake.FailingKubeClient
	err  error
	used bool
}

func (f *failOnce) Update(original, target kube.ResourceList, opts ...kube.ClientUpdateOption) (*kube.Result, error) {
	if !f.used {
		f.used = true
		return &kube.Result{}, f.err
	}
	return f.FailingKubeClient.Update(original, target, opts...)
}

func loadChart(t *testing.T, version string) *chartv2.Chart {
	t.Helper()
	ch, err := loader.LoadDir(chartDir)
	if err != nil {
		t.Fatalf("load chart: %v", err)
	}
	ch.Metadata.Version, ch.Metadata.AppVersion = version, version
	return ch
}

func helmConfig(t *testing.T, kc kube.Interface) *action.Configuration {
	t.Helper()
	caps := common.DefaultCapabilities.Copy()
	kv, err := common.ParseKubeVersion("v1.34.0")
	if err != nil {
		t.Fatal(err)
	}
	caps.KubeVersion = *kv
	if kc == nil {
		kc = &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	}
	cfg := action.NewConfiguration()
	cfg.Releases = storage.Init(driver.NewMemory())
	cfg.KubeClient = kc
	cfg.Capabilities = caps
	return cfg
}

// install instala a versão inicial como o cliente faz hoje (client-side apply) e, para
// simular o Helm 3, apaga o método de aplicação gravado.
func install(t *testing.T, cfg *action.Configuration) {
	t.Helper()
	in := action.NewInstall(cfg)
	in.ReleaseName, in.Namespace, in.ServerSideApply = releaseName, namespace, false
	vals := map[string]any{"connection": map[string]any{"token": token}, "scrapeInterval": "10s"}
	if _, err := in.Run(loadChart(t, "0.1.0"), vals); err != nil {
		t.Fatalf("install: %v", err)
	}
	rel := last(t, cfg)
	rel.ApplyMethod = ""
	if err := cfg.Releases.Update(rel); err != nil {
		t.Fatal(err)
	}
}

func last(t *testing.T, cfg *action.Configuration) *rspb.Release {
	t.Helper()
	r, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*rspb.Release)
}

func deployed(t *testing.T, cfg *action.Configuration) *rspb.Release {
	t.Helper()
	r, err := cfg.Releases.Deployed(releaseName)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*rspb.Release)
}

type harness struct {
	api     *fakeAPI
	cfg     *action.Configuration
	u       *update.Updater
	fetched []wire.AgentUpdateTarget
}

func newHarness(t *testing.T, kc kube.Interface) *harness {
	t.Helper()
	h := &harness{api: &fakeAPI{}, cfg: helmConfig(t, kc)}
	srv := h.api.server(t)
	install(t, h.cfg)
	h.u = &update.Updater{
		API:     update.API{URL: srv.URL, Token: token, Client: srv.Client()},
		Helm:    h.cfg,
		Release: releaseName, Namespace: namespace,
		Fetch: func(_ context.Context, tg wire.AgentUpdateTarget) (*chartv2.Chart, error) {
			h.fetched = append(h.fetched, tg)
			return loadChart(t, tg.Version), nil
		},
		Timeout:      time.Second,
		PendingLimit: 15 * time.Minute,
	}
	return h
}

func (h *harness) wantReports(t *testing.T, want ...wire.AgentUpdateOutcome) []wire.AgentUpdateReport {
	t.Helper()
	if len(h.api.reports) != len(want) {
		t.Fatalf("relatos: quer %v, veio %+v", want, h.api.reports)
	}
	for i, o := range want {
		if h.api.reports[i].Outcome != o {
			t.Fatalf("relato %d: quer %s, veio %+v", i, o, h.api.reports[i])
		}
	}
	return h.api.reports
}

func TestRunNothingToDoLeavesReleaseUntouched(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.api.queried; len(got) != 1 || got[0] != "0.1.0" {
		t.Fatalf("consulta de alvo com a versão instalada: %v", got)
	}
	if h.api.badTokens != 0 {
		t.Fatal("updater não mandou o Bearer do cluster")
	}
	h.wantReports(t)
	if len(h.fetched) != 0 || last(t, h.cfg).Version != 1 {
		t.Fatal("sem alvo, nada pode ser baixado nem aplicado")
	}
}

func TestRunAppliesWholeChartByDigestPreservingValues(t *testing.T) {
	h := newHarness(t, nil)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.fetched) != 1 || h.fetched[0].ChartDigest != "sha256:feed" {
		t.Fatalf("o chart tem de vir pelo digest do alvo: %+v", h.fetched)
	}
	rep := h.wantReports(t, wire.OutcomeApplied)
	if rep[0].Version != "0.2.0" {
		t.Fatalf("relato com a versão errada: %+v", rep[0])
	}
	rel := last(t, h.cfg)
	if rel.Info.Status != rcommon.StatusDeployed || rel.Chart.Metadata.Version != "0.2.0" {
		t.Fatalf("release não subiu: %s %s", rel.Info.Status, rel.Chart.Metadata.Version)
	}
	// Valores do usuário (token incluído) reaproveitados; defaults novos do chart valem.
	if !strings.Contains(rel.Manifest, `token: "`+token+`"`) || !strings.Contains(rel.Manifest, `value: "10s"`) {
		t.Fatal("token ou values do usuário se perderam no upgrade")
	}
	// Release instalada pelo Helm 3 continua em client-side apply.
	if rel.ApplyMethod != string(rspb.ApplyMethodClientSideApply) {
		t.Fatalf("método de aplicação mudou para %q", rel.ApplyMethod)
	}
}

func TestRunRollsBackAndReportsWhenUpgradeFails(t *testing.T) {
	kc := &failOnce{
		FailingKubeClient: &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}},
		err:               errors.New("context deadline exceeded waiting for deployment"),
	}
	h := newHarness(t, kc)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := h.wantReports(t, wire.OutcomeRolledBack)
	if rep[0].Version != "0.2.0" || !strings.Contains(rep[0].Reason, "deadline exceeded") {
		t.Fatalf("relato sem a versão ou o motivo: %+v", rep[0])
	}
	if v := deployed(t, h.cfg).Chart.Metadata.Version; v != "0.1.0" {
		t.Fatalf("release deveria voltar a 0.1.0, está em %s", v)
	}
}

func TestRunReportsRejectedByCeilingOnRBACEscalation(t *testing.T) {
	escalation := apierrors.NewForbidden(
		schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, releaseName,
		errors.New(`user "system:serviceaccount:nuvemcash-system:nuvemcash-agent-updater" (groups=["system:serviceaccounts"]) is attempting to grant RBAC permissions not currently held:
{APIGroups:[""], Resources:["secrets"], Verbs:["list"]}`))
	kc := &failOnce{
		FailingKubeClient: &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}},
		err:               escalation,
	}
	h := newHarness(t, kc)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.3.0", ChartDigest: "sha256:beef"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := h.wantReports(t, wire.OutcomeRejectedByCeiling)
	if rep[0].Version != "0.3.0" {
		t.Fatalf("relato com a versão errada: %+v", rep[0])
	}
	if v := deployed(t, h.cfg).Chart.Metadata.Version; v != "0.1.0" {
		t.Fatalf("release deveria continuar em 0.1.0, está em %s", v)
	}
}

// markPending deixa uma revisão nova presa em pending-upgrade, como um Job morto no meio
// do upgrade.
func markPending(t *testing.T, cfg *action.Configuration, age time.Duration) {
	t.Helper()
	cur := last(t, cfg)
	stuck := *cur
	stuck.Info = &rspb.Info{Status: rcommon.StatusPendingUpgrade, LastDeployed: time.Now().Add(-age)}
	stuck.Version = cur.Version + 1
	stuck.Chart = loadChart(t, "0.2.0")
	if err := cfg.Releases.Create(&stuck); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecoversReleaseStuckPendingBeforeRetrying(t *testing.T) {
	h := newHarness(t, nil)
	markPending(t, h.cfg, time.Hour)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.api.queried; len(got) != 1 || got[0] != "0.1.0" {
		t.Fatalf("a versão instalada é a da última revisão deployed: %v", got)
	}
	h.wantReports(t, wire.OutcomeApplied)
	if rel := last(t, h.cfg); rel.Info.Status != rcommon.StatusDeployed || rel.Chart.Metadata.Version != "0.2.0" {
		t.Fatalf("depois de destravar, o upgrade tinha de subir: %s %s", rel.Info.Status, rel.Chart.Metadata.Version)
	}
}

func TestRunLeavesRecentPendingAlone(t *testing.T) {
	h := newHarness(t, nil)
	markPending(t, h.cfg, time.Minute)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rel := last(t, h.cfg); rel.Info.Status != rcommon.StatusPendingUpgrade {
		t.Fatalf("operação em andamento não pode ser tocada; status %s", rel.Info.Status)
	}
	if len(h.api.queried) != 0 {
		t.Fatal("com operação em andamento, nem consulta o alvo")
	}
}

func TestRunFailsWhenAPIRejectsToken(t *testing.T) {
	h := newHarness(t, nil)
	h.u.API.Token = "wrong"
	if err := h.u.Run(context.Background()); err == nil {
		t.Fatal("401 da api tem de virar erro da execução")
	}
	if last(t, h.cfg).Version != 1 {
		t.Fatal("nada pode ser aplicado sem alvo")
	}
}
