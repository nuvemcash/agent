package main

import (
	"compress/gzip"
	"encoding/json"
	"github.com/nuvemcash/agent/internal/aggregate"
	"github.com/nuvemcash/agent/internal/collect"
	"github.com/nuvemcash/agent/internal/ship"
	"github.com/nuvemcash/agent/wire"
	"io"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// O liveness precisa passar desde o primeiro instante, senão o kubelet reinicia o agente
// antes de ele terminar de sincronizar os caches; o readiness é que espera a sincronização.
func TestProbesSeparamVivoDePronto(t *testing.T) {
	var ready atomic.Bool
	mux := probeMux(&ready, ship.New("http://unused", "tok", 10, 0), &collectionHealth{}, true)

	get := func(path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}

	if got := get("/healthz"); got != http.StatusOK {
		t.Fatalf("healthz antes do sync = %d, quer %d", got, http.StatusOK)
	}
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz antes do sync = %d, quer %d", got, http.StatusServiceUnavailable)
	}

	ready.Store(true)
	if got := get("/readyz"); got != http.StatusOK {
		t.Fatalf("readyz depois do sync = %d, quer %d", got, http.StatusOK)
	}
}

func TestTrimReplicaSetTemplate(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc123",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "web", Controller: ptr(true)},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "x:1"}}},
			},
		},
	}
	out, err := trimCached(rs)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	got, ok := out.(*appsv1.ReplicaSet)
	if !ok {
		t.Fatalf("tipo devolvido = %T, quer *appsv1.ReplicaSet", out)
	}
	// O template é o que se joga fora; as ownerReferences são o motivo de o RS estar no
	// cache (resolução Pod→ReplicaSet→Deployment) e têm de sobreviver intactas.
	if len(got.Spec.Template.Spec.Containers) != 0 {
		t.Errorf("template não foi descartado: %+v", got.Spec.Template)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "web" {
		t.Errorf("ownerReferences perdidas: %+v", got.OwnerReferences)
	}
}

// A poda do Pod é a mais perigosa do transform: tirar demais quebra a resolução
// Pod→RS→Deployment em SILÊNCIO. Este teste fixa exatamente o que ResolvePodMeta consome —
// se alguém podar um desses campos, quebra aqui e não em produção.
func TestTrimCachedPreservaOQueAAgregacaoLe(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc-1", Namespace: "prod",
			Labels:      map[string]string{"app": "web", "pod-template-hash": "abc"},
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("x", 4096)},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "web-abc", Controller: ptr(true)},
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{
				Name: "app", Image: "x:1",
				Env:       []corev1.EnvVar{{Name: "SEGREDO", Value: strings.Repeat("y", 2048)}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Message: strings.Repeat("z", 4096)},
	}

	out, err := trimCached(pod)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	got, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("tipo devolvido = %T", out)
	}

	// Sobrevive: tudo o que ResolvePodMeta lê.
	if got.Namespace != "prod" || got.Name != "web-abc-1" || got.Spec.NodeName != "node-1" {
		t.Errorf("identidade/nó perdidos: %+v", got.ObjectMeta)
	}
	if got.Labels["app"] != "web" {
		t.Errorf("labels perdidas: %+v", got.Labels)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "web-abc" {
		t.Errorf("ownerReferences perdidas — Pod→RS→Deployment quebra em silêncio: %+v", got.OwnerReferences)
	}
	if len(got.Spec.Containers) != 1 ||
		got.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() != 250 {
		t.Errorf("requests perdidos: %+v", got.Spec.Containers)
	}

	// Some: o peso morto.
	if got.Status.Message != "" || got.Annotations != nil || got.ManagedFields != nil ||
		len(got.Spec.Containers[0].Env) != 0 {
		t.Errorf("poda não removeu o peso morto: status=%q anns=%v mf=%v env=%v",
			got.Status.Message, got.Annotations, got.ManagedFields, got.Spec.Containers[0].Env)
	}
}

// A factory aplica o transform a TODOS os informers — tipos sem regra própria só perdem os
// managedFields.
func TestTrimCachedOutrosTiposSoPerdemManagedFields(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "s1", Namespace: "prod",
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
	}}
	out, err := trimCached(svc)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	got := out.(*corev1.Service)
	if got.Name != "s1" || got.Namespace != "prod" {
		t.Errorf("service alterado além do esperado: %+v", got.ObjectMeta)
	}
	if got.ManagedFields != nil {
		t.Error("managedFields deviam ter sido descartados")
	}
}

func ptr[T any](v T) *T { return &v }

// O scrape HTTP local deve mostrar retenção e recuperação do envio real.
func TestShippingMetrics(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer ingest.Close()
	s := ship.New(ingest.URL, "tok", 10, 0)
	var ready atomic.Bool
	mux := probeMux(&ready, s, &collectionHealth{}, true)
	metric := func(name string) float64 {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("metrics status: %d", rec.Code)
		}
		return metricValue(t, rec.Body.String(), name)
	}
	if err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if metric("nuvemcash_agent_ship_failed") != 0 || metric("nuvemcash_agent_buffer_windows") != 0 || metric("nuvemcash_agent_ship_last_success_timestamp_seconds") != 0 {
		t.Fatal("ociosidade não é envio impedido nem conclusão de envio")
	}
	s.Enqueue(wire.Snapshot{WindowStart: time.Now().Add(-5 * time.Minute), WindowEnd: time.Now()})
	if err := s.Flush(t.Context()); err == nil {
		t.Fatal("esperava 503")
	}
	if metric("nuvemcash_agent_ship_failed") != 1 || metric("nuvemcash_agent_buffer_windows") != 1 || metric("nuvemcash_agent_buffer_bytes") <= 0 || metric("nuvemcash_agent_buffer_oldest_age_seconds") <= 0 || metric("nuvemcash_agent_ship_failure_since_timestamp_seconds") <= 0 {
		t.Fatal("a janela impedida deve permanecer observável")
	}
	status.Store(http.StatusAccepted)
	if err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if metric("nuvemcash_agent_ship_failed") != 0 || metric("nuvemcash_agent_buffer_windows") != 0 || metric("nuvemcash_agent_buffer_bytes") != 0 || metric("nuvemcash_agent_buffer_oldest_age_seconds") != 0 || metric("nuvemcash_agent_ship_last_success_timestamp_seconds") <= 0 {
		t.Fatal("drenagem aceita deve recuperar o envio")
	}
}

func metricValue(t *testing.T, body, name string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("métrica %q ausente em:\n%s", name, body)
	return 0
}

func TestMetricsPartialCollectionNotRecoveredByShipping(t *testing.T) {
	var fail atomic.Value
	fail.Store("")
	fixture, err := os.ReadFile("../../internal/collect/testdata/summary.json")
	if err != nil {
		t.Fatal(err)
	}
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if node := fail.Load().(string); node != "" && strings.Contains(r.URL.Path, "/nodes/"+node+"/") {
			http.Error(w, "kubelet indisponível", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer kube.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: kube.URL})
	if err != nil {
		t.Fatal(err)
	}
	var received wire.Snapshot
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() {
			if err := zr.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := json.NewDecoder(zr).Decode(&received); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ingest.Close()
	s := ship.New(ingest.URL, "tok", 10, 0)
	h := &collectionHealth{}
	var ready atomic.Bool
	ready.Store(true)
	mux := probeMux(&ready, s, h, true)
	metric := func(name string) float64 {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return metricValue(t, rec.Body.String(), name)
	}
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}, {ObjectMeta: metav1.ObjectMeta{Name: "node-2"}}}
	scrapeNodes(t.Context(), client, nodes, h)
	lastSuccess := metric("nuvemcash_agent_collection_last_success_timestamp_seconds")
	if lastSuccess <= 0 || metric("nuvemcash_agent_collection_complete") != 1 {
		t.Fatal("coleta completa não observada")
	}
	fail.Store("node-1")
	partial := scrapeNodes(t.Context(), client, nodes, h)
	if len(partial) != 1 {
		t.Fatalf("coleta parcial: %d nós", len(partial))
	}
	window := aggregate.NewWindow(time.Now().Add(-5 * time.Minute))
	for _, result := range partial {
		window.ObserveNode(result.node, result.at)
		for _, sample := range result.samples {
			meta := aggregate.PodMeta{Node: result.node, Namespace: sample.Namespace, WorkloadKind: "Pod", WorkloadName: sample.PodName}
			window.Observe(sample, meta)
			sample.Time = sample.Time.Add(time.Minute)
			sample.CPUUsageCoreSeconds++
			window.Observe(sample, meta)
		}
	}
	s.Enqueue(wire.Snapshot{WindowStart: window.Start(), WindowEnd: time.Now(), Nodes: collect.NodeInventory(nodes), Usage: window.Close(time.Now())})
	if err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(received.Nodes) != 2 || len(received.Usage) != 1 || received.Usage[0].Node != "node-2" {
		t.Fatalf("envio deve conter o inventário e somente o uso coletado: %+v", received)
	}
	if metric("nuvemcash_agent_collection_failed") != 1 || metric("nuvemcash_agent_collection_complete") != 0 || metric("nuvemcash_agent_collection_nodes_collected") != 1 || metric("nuvemcash_agent_collection_nodes_expected") != 2 {
		t.Fatal("envio aceito apagou a falha parcial")
	}
	if metric("nuvemcash_agent_collection_last_success_timestamp_seconds") != lastSuccess {
		t.Fatal("parcial não é conclusão válida")
	}
	failedSince := metric("nuvemcash_agent_collection_failure_since_timestamp_seconds")
	scrapeNodes(t.Context(), client, nodes, h)
	if metric("nuvemcash_agent_collection_failure_since_timestamp_seconds") != failedSince {
		t.Fatal("retry reiniciou a falha do nó")
	}
	fail.Store("node-2")
	scrapeNodes(t.Context(), client, nodes, h)
	if metric("nuvemcash_agent_collection_failure_since_timestamp_seconds") <= failedSince {
		t.Fatal("recuperação do nó deve encerrar apenas a falha dele")
	}
	fail.Store("")
	scrapeNodes(t.Context(), client, nodes, h)
	if metric("nuvemcash_agent_collection_failed") != 0 || metric("nuvemcash_agent_collection_complete") != 1 || metric("nuvemcash_agent_collection_last_success_timestamp_seconds") <= lastSuccess {
		t.Fatal("coleta completa não recuperou")
	}
}

func TestMetricsPermanentLosses(t *testing.T) {
	cases := []struct {
		name, reason                 string
		maxWindows, maxBytes, status int
		encode                       bool
	}{
		{name: "limite de janelas", reason: "buffer_windows", maxWindows: 1},
		{name: "limite de bytes", reason: "buffer_bytes", maxWindows: 10, maxBytes: 1},
		{name: "encode inválido", reason: "encode", maxWindows: 10, encode: true},
		{name: "rejeição HTTP", reason: "http_rejected", maxWindows: 10, status: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			defer ingest.Close()
			s := ship.New(ingest.URL, "tok", tc.maxWindows, tc.maxBytes)
			var ready atomic.Bool
			mux := probeMux(&ready, s, &collectionHealth{}, true)
			metrics := func() string {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				return rec.Body.String()
			}
			counter := `nuvemcash_agent_dropped_windows_total{reason="` + tc.reason + `"}`
			lastDrop := `nuvemcash_agent_last_drop_timestamp_seconds{reason="` + tc.reason + `"}`
			if metricValue(t, metrics(), counter) != 0 || metricValue(t, metrics(), lastDrop) != 0 {
				t.Fatal("processo novo não tem perdas históricas")
			}
			snap := wire.Snapshot{WindowStart: time.Now().Add(-5 * time.Minute), WindowEnd: time.Now()}
			if tc.encode {
				snap.WindowEnd = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			s.Enqueue(snap)
			if tc.status != 0 {
				if err := s.Flush(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if !tc.encode {
				s.Enqueue(snap)
			}
			body := metrics()
			if metricValue(t, body, counter) != 1 || metricValue(t, body, lastDrop) <= 0 {
				t.Fatal("perda definitiva não observada")
			}
			if metricValue(t, body, "nuvemcash_agent_ship_failed") != 0 {
				t.Fatal("descarte não é retenção recuperável")
			}
			if metricValue(t, body, "nuvemcash_agent_ship_last_success_timestamp_seconds") != 0 {
				t.Fatal("descarte não é envio aceito")
			}
			if s.Dropped() != 1 {
				t.Fatal("autotelemetria deve contar todas as perdas")
			}
		})
	}
}

func TestMetricsShippingRecoversOnlyAffectedWindow(t *testing.T) {
	var calls atomic.Int32
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1, 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer ingest.Close()
	s := ship.New(ingest.URL, "tok", 10, 0)
	var ready atomic.Bool
	mux := probeMux(&ready, s, &collectionHealth{}, true)
	metrics := func() string {
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return r.Body.String()
	}
	s.Enqueue(wire.Snapshot{WindowStart: time.Now().Add(-10 * time.Minute), WindowEnd: time.Now().Add(-5 * time.Minute)})
	s.Enqueue(wire.Snapshot{WindowStart: time.Now().Add(-5 * time.Minute), WindowEnd: time.Now()})
	if err := s.Flush(t.Context()); err == nil {
		t.Fatal("esperava primeira falha")
	}
	before := metricValue(t, metrics(), "nuvemcash_agent_ship_failure_since_timestamp_seconds")
	if err := s.Flush(t.Context()); err == nil {
		t.Fatal("a segunda janela deve falhar após aceite da primeira")
	}
	body := metrics()
	if metricValue(t, body, "nuvemcash_agent_ship_failed") != 1 || metricValue(t, body, "nuvemcash_agent_buffer_windows") != 1 || metricValue(t, body, "nuvemcash_agent_ship_failure_since_timestamp_seconds") <= before {
		t.Fatal("falhas de janelas distintas foram confundidas")
	}
	if err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if metricValue(t, metrics(), "nuvemcash_agent_ship_failed") != 0 {
		t.Fatal("drenagem final não recuperou")
	}
}

func TestMetricsConcurrentHTTPAndPrometheusFormat(t *testing.T) {
	tool := os.Getenv("PROMTOOL")
	if tool == "" {
		tool = "promtool"
	}
	if _, err := exec.LookPath(tool); err != nil {
		t.Skip("promtool necessário para validar a exposição HTTP")
	}
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer ingest.Close()
	s := ship.New(ingest.URL, "tok", 2, 0)
	var ready atomic.Bool
	health := &collectionHealth{}
	server := httptest.NewServer(probeMux(&ready, s, health, true))
	defer server.Close()
	errors := make(chan error, 1)
	go func() {
		for range 20 {
			response, err := http.Get(server.URL + "/metrics")
			if err != nil {
				errors <- err
				return
			}
			_, err = io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				errors <- err
				return
			}
		}
		errors <- nil
	}()
	for range 20 {
		s.Enqueue(wire.Snapshot{WindowStart: time.Now().Add(-5 * time.Minute), WindowEnd: time.Now()})
		if err := s.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatal("contrato HTTP das métricas inválido")
	}
	cmd := exec.Command(tool, "check", "metrics")
	cmd.Stdin = response.Body
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("promtool: %v\n%s", err, output)
	}
	optOut := httptest.NewRecorder()
	probeMux(&ready, s, health, false).ServeHTTP(optOut, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if optOut.Code != http.StatusNotFound {
		t.Fatal("endpoint ignorou opt-out")
	}
}
