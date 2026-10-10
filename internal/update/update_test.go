package update_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

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

// managerAtUpdate grava o field manager vigente no momento do apply.
type managerAtUpdate struct {
	*kubefake.FailingKubeClient
	got string
}

func (m *managerAtUpdate) Update(original, target kube.ResourceList, opts ...kube.ClientUpdateOption) (*kube.Result, error) {
	m.got = kube.ManagedFieldsManager
	return m.FailingKubeClient.Update(original, target, opts...)
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
	kube    *fake.Clientset
	u       *update.Updater
	fetched []wire.AgentUpdateTarget
	// verified registra cada alvo verificado; verifyErr é o desfecho da Verificação de origem.
	verified  []wire.AgentUpdateTarget
	verifyErr error
}

const defaultImage = "ghcr.io/nuvemcash/agent"

// agentDeployment é o coletor como o chart o renderiza; opts mexem em labels, annotations
// ou imagem antes de ele entrar no clientset falso.
func agentDeployment(image string, opts ...func(*metav1.ObjectMeta)) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "agent", Image: image}}
	for _, o := range opts {
		o(&d.ObjectMeta)
	}
	return d
}

func updaterCronJob(opts ...func(*metav1.ObjectMeta)) *batchv1.CronJob {
	c := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: releaseName + "-updater", Namespace: namespace}}
	for _, o := range opts {
		o(&c.ObjectMeta)
	}
	return c
}

func withLabel(k, v string) func(*metav1.ObjectMeta) {
	return func(m *metav1.ObjectMeta) {
		if m.Labels == nil {
			m.Labels = map[string]string{}
		}
		m.Labels[k] = v
	}
}

func withAnnotation(k, v string) func(*metav1.ObjectMeta) {
	return func(m *metav1.ObjectMeta) {
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[k] = v
	}
}

func newHarness(t *testing.T, kc kube.Interface) *harness {
	t.Helper()
	return newHarnessWith(t, kc, agentDeployment(defaultImage+":0.1.0"), updaterCronJob())
}

func newHarnessWith(t *testing.T, kc kube.Interface, live ...any) *harness {
	t.Helper()
	h := &harness{api: &fakeAPI{}, cfg: helmConfig(t, kc), kube: fake.NewClientset()}
	for _, o := range live {
		var err error
		switch o := o.(type) {
		case *appsv1.Deployment:
			_, err = h.kube.AppsV1().Deployments(namespace).Create(context.Background(), o, metav1.CreateOptions{})
		case *batchv1.CronJob:
			_, err = h.kube.BatchV1().CronJobs(namespace).Create(context.Background(), o, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	h.kube.ClearActions()
	srv := h.api.server(t)
	install(t, h.cfg)
	h.u = &update.Updater{
		API:     update.API{URL: srv.URL, Token: token, Client: srv.Client()},
		Helm:    h.cfg,
		Kube:    h.kube,
		Release: releaseName, Namespace: namespace,
		Fetch: func(_ context.Context, tg wire.AgentUpdateTarget) (*chartv2.Chart, error) {
			h.fetched = append(h.fetched, tg)
			return loadChart(t, tg.Version), nil
		},
		Verify: func(_ context.Context, tg wire.AgentUpdateTarget) error {
			h.verified = append(h.verified, tg)
			return h.verifyErr
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
	if len(h.verified) != 1 || h.verified[0] != h.fetched[0] {
		t.Fatalf("a origem do alvo tem de ser verificada antes do upgrade: %+v", h.verified)
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

// Assinatura inválida: nenhum upgrade, relato signature_invalid com o motivo.
func TestRunReportsSignatureInvalidAndLeavesReleaseUntouched(t *testing.T) {
	h := newHarness(t, nil)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	h.verifyErr = fmt.Errorf("%w: SAN mismatch v0.2.1", update.ErrSignatureInvalid)
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := h.wantReports(t, wire.OutcomeSignatureInvalid)
	if rep[0].Version != "0.2.0" || !strings.Contains(rep[0].Reason, "SAN mismatch v0.2.1") {
		t.Fatalf("relato sem a versão ou o motivo: %+v", rep[0])
	}
	if rel := last(t, h.cfg); rel.Version != 1 || rel.Chart.Metadata.Version != "0.1.0" {
		t.Fatalf("release não pode ser tocada: revisão %d, %s", rel.Version, rel.Chart.Metadata.Version)
	}
}

// Erro transitório (rede, 5xx do GHCR): sem relato nem upgrade; a próxima hora tenta de novo.
func TestRunTransientVerifyErrorDoesNotReport(t *testing.T) {
	h := newHarness(t, nil)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	h.verifyErr = errors.New("ghcr: 503 service unavailable")
	if err := h.u.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("erro transitório tem de voltar como erro da rodada: %v", err)
	}
	h.wantReports(t)
	if rel := last(t, h.cfg); rel.Version != 1 {
		t.Fatalf("release não pode ser tocada: revisão %d", rel.Version)
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

// Um helm upgrade manual que começa entre a consulta e a aplicação não é falha da versão:
// relatar rolled_back tiraria a versão das ofertas a este cluster para sempre.
func TestRunConcurrentOperationIsNotAFailure(t *testing.T) {
	h := newHarness(t, nil)
	fetch := h.u.Fetch
	h.u.Fetch = func(ctx context.Context, tg wire.AgentUpdateTarget) (*chartv2.Chart, error) {
		markPending(t, h.cfg, 0)
		return fetch(ctx, tg)
	}
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantReports(t)
}

// A abstenção não escreve nada: nem no cluster (só leituras no clientset), nem na release,
// nem consulta o alvo — só relata o motivo.
func (h *harness) wantAbstained(t *testing.T, reason, version string) {
	t.Helper()
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := h.wantReports(t, wire.OutcomeAbstained)
	if rep[0].Reason != reason || rep[0].Version != version {
		t.Fatalf("relato: quer %s em %s, veio %+v", reason, version, rep[0])
	}
	if len(h.api.queried) != 0 || len(h.fetched) != 0 || last(t, h.cfg).Version != 1 {
		t.Fatalf("abstenção não pode consultar, baixar nem aplicar: %v %v", h.api.queried, h.fetched)
	}
	for _, a := range h.kube.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Fatalf("abstenção escreveu no cluster: %v", a)
		}
	}
}

func TestRunAbstainsUnderFluxOwnership(t *testing.T) {
	for name, live := range map[string][]any{
		"deployment": {agentDeployment(defaultImage+":0.1.0", withLabel("helm.toolkit.fluxcd.io/name", "agent")), updaterCronJob()},
		"cronjob":    {agentDeployment(defaultImage + ":0.1.0"), updaterCronJob(withLabel("helm.toolkit.fluxcd.io/namespace", "flux-system"))},
		"annotation": {agentDeployment(defaultImage+":0.1.0", withAnnotation("helm.toolkit.fluxcd.io/driftDetection", "x")), updaterCronJob()},
	} {
		t.Run(name, func(t *testing.T) {
			newHarnessWith(t, nil, live...).wantAbstained(t, wire.ReasonGitOpsFlux, "0.1.0")
		})
	}
}

func TestRunAbstainsUnderArgoOwnership(t *testing.T) {
	for name, meta := range map[string]func(*metav1.ObjectMeta){
		"tracking-id": withAnnotation("argocd.argoproj.io/tracking-id", "agent:apps/Deployment:ns/agent"),
		"instance":    withLabel("argocd.argoproj.io/instance", "agent"),
	} {
		t.Run(name, func(t *testing.T) {
			newHarnessWith(t, nil, agentDeployment(defaultImage+":0.1.0", meta), updaterCronJob()).
				wantAbstained(t, wire.ReasonGitOpsArgo, "0.1.0")
		})
	}
}

// O Argo renderiza o chart e aplica os manifestos: não existe release do Helm no cluster.
// A abstenção não pode depender dela, e a versão sai da tag da imagem.
func TestRunAbstainsUnderArgoWithoutHelmRelease(t *testing.T) {
	h := newHarnessWith(t, nil,
		agentDeployment(defaultImage+":0.1.7@sha256:abc", withLabel("argocd.argoproj.io/instance", "agent")), updaterCronJob())
	if _, err := h.cfg.Releases.Delete(releaseName, 1); err != nil {
		t.Fatal(err)
	}
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep := h.wantReports(t, wire.OutcomeAbstained); rep[0].Reason != wire.ReasonGitOpsArgo || rep[0].Version != "0.1.7" {
		t.Fatalf("relato: %+v", rep[0])
	}
}

func TestRunAbstainsOnMirroredRegistry(t *testing.T) {
	for _, image := range []string{
		"registry.interno.example/nuvemcash/agent:0.1.0",
		"localhost:5000/agent:0.1.0",
		"registry.interno.example/agent@sha256:abc",
	} {
		t.Run(image, func(t *testing.T) {
			newHarnessWith(t, nil, agentDeployment(image), updaterCronJob()).
				wantAbstained(t, wire.ReasonMirroredRegistry, "0.1.0")
		})
	}
}

func TestRunDoesNotAbstainOnDefaultRegistryOrUnrelatedMetadata(t *testing.T) {
	for name, image := range map[string]string{"tag": defaultImage + ":0.1.0", "digest": defaultImage + ":0.1.0@sha256:abc", "no tag": defaultImage} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessWith(t, nil,
				agentDeployment(image, withLabel("app.kubernetes.io/instance", "agent"), withAnnotation("helm.sh/resource-policy", "keep")), updaterCronJob())
			h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
			if err := h.u.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.wantReports(t, wire.OutcomeApplied)
		})
	}
}

// O apply sai como o helm CLI ("helm"): com o manager do binário ("agent") o SSA conflita
// com a release que o cliente atualizou à mão pelo Helm 4 (api#316).
func TestRunAppliesWithHelmCLIFieldManager(t *testing.T) {
	prev := kube.ManagedFieldsManager
	t.Cleanup(func() { kube.ManagedFieldsManager = prev })
	kube.ManagedFieldsManager = ""
	kc := &managerAtUpdate{FailingKubeClient: &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}}
	h := newHarness(t, kc)
	h.api.target = &wire.AgentUpdateTarget{Version: "0.2.0", ChartDigest: "sha256:feed"}
	if err := h.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantReports(t, wire.OutcomeApplied)
	if kc.got != "helm" {
		t.Fatalf("field manager no apply: quer %q, veio %q", "helm", kc.got)
	}
}
