package aggregate

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nuvemcash/agent/internal/collect"
)

func meta(ns, name string) PodMeta {
	return PodMeta{Namespace: ns, Name: name, Node: "n1",
		CPURequestMilli: 100, MemoryRequestBytes: 1 << 30,
		WorkloadKind: "Deployment", WorkloadName: "web"}
}

func sample(pod string, at time.Time, cpuCum float64, ws int64) collect.PodSample {
	return collect.PodSample{Namespace: "app", PodName: pod, PodUID: pod,
		Time: at, CPUUsageCoreSeconds: cpuCum, WorkingSetBytes: ws}
}

func TestWindow_DeltaEIntegrais(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	w.Observe(sample("p1", t0, 100.0, 1<<30), meta("app", "p1"))
	w.Observe(sample("p1", t0.Add(60*time.Second), 106.0, 1<<30), meta("app", "p1"))

	rows := w.Close(t0.Add(5 * time.Minute))
	if len(rows) != 1 {
		t.Fatalf("esperava 1 linha, veio %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.WorkloadKind != "Deployment" || r.WorkloadName != "web" || r.Node != "n1" {
		t.Fatalf("identidade errada: %+v", r)
	}
	if r.CPUUsageCoreSeconds != 6.0 { // 106-100
		t.Fatalf("Δ cpu errado: %v", r.CPUUsageCoreSeconds)
	}
	if r.CoverageSeconds != 60 {
		t.Fatalf("coverage errada: %d", r.CoverageSeconds)
	}
	if r.CPURequestMilliSeconds != 100*60 { // 100m × 60s
		t.Fatalf("integral de request errada: %d", r.CPURequestMilliSeconds)
	}
	if r.MemoryWorkingSetByteSeconds != float64(1<<30)*60 {
		t.Fatalf("integral de ws errada: %v", r.MemoryWorkingSetByteSeconds)
	}
}

func TestWindow_DoisPodsMesmoWorkloadSomam(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	for _, p := range []string{"p1", "p2"} {
		w.Observe(sample(p, t0, 10, 1<<20), meta("app", p))
		w.Observe(sample(p, t0.Add(30*time.Second), 13, 1<<20), meta("app", p))
	}
	rows := w.Close(t0.Add(5 * time.Minute))
	if len(rows) != 1 || rows[0].CPUUsageCoreSeconds != 6.0 || rows[0].CoverageSeconds != 60 {
		t.Fatalf("soma por workload errada: %+v", rows)
	}
}

func TestWindow_RestartNaoGeraDeltaNegativo(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	w.Observe(sample("p1", t0, 500, 1<<20), meta("app", "p1"))
	w.Observe(sample("p1", t0.Add(60*time.Second), 2, 1<<20), meta("app", "p1")) // cumulativo regrediu
	rows := w.Close(t0.Add(5 * time.Minute))
	if rows[0].CPUUsageCoreSeconds != 0 {
		t.Fatalf("restart devia zerar o Δ: %+v", rows[0])
	}
	if rows[0].CoverageSeconds != 60 { // dt continua contando (pod existia)
		t.Fatalf("coverage no restart: %+v", rows[0])
	}
}

func TestWindow_CarregaUltimaAmostraParaProximaJanela(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	w.Observe(sample("p1", t0.Add(4*time.Minute), 100, 1<<20), meta("app", "p1"))
	_ = w.Close(t0.Add(5 * time.Minute))

	w2 := NewWindowFrom(w, t0.Add(5*time.Minute))
	w2.Observe(sample("p1", t0.Add(6*time.Minute), 130, 1<<20), meta("app", "p1"))
	rows := w2.Close(t0.Add(10 * time.Minute))
	if rows[0].CPUUsageCoreSeconds != 30 || rows[0].CoverageSeconds != 120 {
		t.Fatalf("continuidade entre janelas falhou: %+v", rows[0])
	}
}

func TestWindow_NodeSampledSeconds(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	// A cobertura do nó vem do SCRAPE do nó: o 1º só estabelece a base, o 2º fecha 45s.
	w.ObserveNode("n1", t0)
	w.ObserveNode("n1", t0.Add(45*time.Second))

	got := w.NodeSampledSeconds()
	if got["n1"] != 45 {
		t.Fatalf("cobertura do nó errada: %+v", got)
	}
}

func TestWindow_NodeSampledSecondsNaoMultiplicaPorPod(t *testing.T) {
	// Regressão do dogfood (13/08/2026): a cobertura era somada dentro de Observe, que roda
	// uma vez por POD — um nó com 72 pods reportava 72× a duração real da janela. Com
	// sampled_s estourando a hora, o backend clampa em 3600 e NUNCA emite "período não
	// monitorado", que é justamente o que distingue ociosidade real de hora não observada.
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	w.ObserveNode("n1", t0)
	w.ObserveNode("n1", t0.Add(45*time.Second))
	for _, pod := range []string{"p1", "p2", "p3"} {
		w.Observe(sample(pod, t0, 10, 1<<20), meta("app", pod))
		w.Observe(sample(pod, t0.Add(45*time.Second), 13, 1<<20), meta("app", pod))
	}

	if got := w.NodeSampledSeconds(); got["n1"] != 45 {
		t.Fatalf("cobertura do nó = %v; 3 pods não podem triplicar a janela de 45s", got)
	}
}

func TestWindow_NodeSampledSecondsContinuaEntreJanelas(t *testing.T) {
	// Sem carregar o último scrape para a janela nova, o 1º scrape de cada janela só
	// estabeleceria a base e a cobertura perderia um intervalo inteiro por janela.
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w1 := NewWindow(t0)
	w1.ObserveNode("n1", t0)
	w1.ObserveNode("n1", t0.Add(60*time.Second))

	w2 := NewWindowFrom(w1, t0.Add(90*time.Second))
	w2.ObserveNode("n1", t0.Add(120*time.Second))

	if got := w2.NodeSampledSeconds(); got["n1"] != 60 {
		t.Fatalf("cobertura da 2ª janela = %v, quer 60 (intervalo desde o scrape anterior)", got)
	}
}

func TestWindow_CoverageSemViesDeTruncamento(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	dt := 600 * time.Millisecond
	for i := range 4 {
		w.Observe(sample("p1", t0.Add(time.Duration(i)*dt), float64(i), 1<<20), meta("app", "p1"))
	}
	rows := w.Close(t0.Add(5 * time.Minute))
	if len(rows) != 1 {
		t.Fatalf("esperava 1 linha, veio %d", len(rows))
	}
	// 3 deltas de 0.6s cada = 1.8s reais. Truncando int64(dt) por amostra (0.6→0) a soma
	// ficaria 0; acumulando em float64 e arredondando 1x no Close dá round(1.8)=2.
	if rows[0].CoverageSeconds != 2 {
		t.Fatalf("coverage deveria acumular sem viés de truncamento por amostra: got %d, want 2", rows[0].CoverageSeconds)
	}
}

func TestNewWindowFrom_HorizonteDescartaAmostraVelha(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	w := NewWindow(t0)
	w.Observe(sample("old", t0, 10, 1<<20), meta("app", "old"))                          // Time = t0
	w.Observe(sample("recent", t0.Add(4*time.Minute), 10, 1<<20), meta("app", "recent")) // Time = t0+4m
	_ = w.Close(t0.Add(5 * time.Minute))

	newStart := t0.Add(5 * time.Minute) // horizonte de corte: newStart - 3min = t0+2min
	w2 := NewWindowFrom(w, newStart)

	if _, ok := w2.prev["old"]; ok {
		t.Fatalf("amostra fora do horizonte (t0) deveria ter sido descartada na rolagem")
	}
	if _, ok := w2.prev["recent"]; !ok {
		t.Fatalf("amostra dentro do horizonte (t0+4m) deveria sobreviver à rolagem")
	}
}

func TestResolvePodMeta(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "app", Name: "web-abc123",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}},
	}}
	podLabels := map[string]string{"app": "web", "pod-template-hash": "abc123"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-abc123-x",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc123"}},
			Labels:          podLabels},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			}},
		}}},
	}
	m := ResolvePodMeta(pod, map[string]*appsv1.ReplicaSet{"app/web-abc123": rs})
	if m.WorkloadKind != "Deployment" || m.WorkloadName != "web" || m.CPURequestMilli != 250 {
		t.Fatalf("resolução errada: %+v", m)
	}
	if m.Labels["app"] != "web" {
		t.Fatalf("label 'app' deveria sobreviver ao filtro: %+v", m.Labels)
	}
	if _, hasHash := m.Labels["pod-template-hash"]; hasHash {
		t.Fatalf("pod-template-hash deveria ser filtrado: %+v", m.Labels)
	}
	if len(podLabels) != 2 || podLabels["pod-template-hash"] != "abc123" {
		t.Fatalf("map de labels do pod original não pode ser mutado: %+v", podLabels)
	}

	bare := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "solo"},
		Spec: corev1.PodSpec{NodeName: "n1"}}
	m2 := ResolvePodMeta(bare, nil)
	if m2.WorkloadKind != "Pod" || m2.WorkloadName != "solo" || m2.CPURequestMilli != 0 {
		t.Fatalf("bare pod errado: %+v", m2)
	}
}
