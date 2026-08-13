// Package aggregate acumula amostras pod-level em linhas por (nó, workload) por janela.
// O agente NÃO calcula custo nem max(request, uso) — isso é do backend (spec, decisão 14).
package aggregate

import (
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nuvemcash/agent/internal/collect"
	"github.com/nuvemcash/agent/wire"
)

// PodMeta é a identidade de rateio de um pod (dono topo + requests somados dos containers).
type PodMeta struct {
	Namespace          string
	Name               string
	Node               string
	CPURequestMilli    int64
	MemoryRequestBytes int64
	WorkloadKind       string
	WorkloadName       string
	Labels             map[string]string
}

// ResolvePodMeta resolve o controlador TOPO por ownerReferences.
// Pod→ReplicaSet→Deployment; Job→CronJob; STS/DS/Job diretos; sem dono = "Pod".
// rsByKey é o índice "ns/nome"→ReplicaSet vindo do lister (para achar o Deployment).
func ResolvePodMeta(pod *corev1.Pod, rsByKey map[string]*appsv1.ReplicaSet) PodMeta {
	m := PodMeta{
		Namespace: pod.Namespace, Name: pod.Name, Node: pod.Spec.NodeName,
		WorkloadKind: "Pod", WorkloadName: pod.Name,
	}
	// Labels do pod são a fonte de classificação (padrão GKE/SCAD); hashes de template
	// são ruído por-ReplicaSet/por-revisão e sairiam como cardinalidade falsa nas tags.
	if len(pod.Labels) > 0 {
		m.Labels = make(map[string]string, len(pod.Labels))
		for k, v := range pod.Labels {
			switch k {
			case "pod-template-hash", "controller-revision-hash", "pod-template-generation":
				continue
			}
			m.Labels[k] = v
		}
		if len(m.Labels) == 0 {
			m.Labels = nil
		}
	}
	for _, c := range pod.Spec.Containers {
		m.CPURequestMilli += c.Resources.Requests.Cpu().MilliValue()
		m.MemoryRequestBytes += c.Resources.Requests.Memory().Value()
	}
	if owner := controllerOf(pod.OwnerReferences); owner != nil {
		m.WorkloadKind, m.WorkloadName = owner.Kind, owner.Name
		if owner.Kind == "ReplicaSet" {
			if rs, ok := rsByKey[pod.Namespace+"/"+owner.Name]; ok {
				if dep := controllerOf(rs.OwnerReferences); dep != nil && dep.Kind == "Deployment" {
					m.WorkloadKind, m.WorkloadName = "Deployment", dep.Name
				}
			}
		}
	}
	return m
}

// controllerOf devolve a ownerReference controladora (ou a primeira, como aproximação).
func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

// Window acumula uma janela de agregação.
type Window struct {
	start time.Time
	prev  map[string]collect.PodSample // última amostra por podUID (para Δ e dt)
	acc   map[string]*wire.WorkloadUsage
}

func NewWindow(start time.Time) *Window {
	return &Window{start: start, prev: map[string]collect.PodSample{}, acc: map[string]*wire.WorkloadUsage{}}
}

// prevSampleHorizon é o quanto uma amostra sobrevive na rolagem entre janelas. ≥ 2× o
// scrape default de 60s: pod sem amostra nesse horizonte está morto (não só atrasado) —
// reter a amostra dele para sempre seria um leak monotônico em cluster com churn de pods.
const prevSampleHorizon = 3 * time.Minute

// NewWindowFrom preserva as últimas amostras da janela anterior (continuidade do Δ),
// descartando as que já saíram do horizonte de retenção.
func NewWindowFrom(prev *Window, start time.Time) *Window {
	w := NewWindow(start)
	if prev != nil {
		cutoff := start.Add(-prevSampleHorizon)
		for k, v := range prev.prev {
			if v.Time.After(cutoff) {
				w.prev[k] = v
			}
		}
	}
	return w
}

// Observe registra uma amostra. O Δ é contra a amostra anterior do MESMO pod; regressão
// do cumulativo (restart) zera o Δ daquele intervalo mas mantém a cobertura.
func (w *Window) Observe(s collect.PodSample, m PodMeta) {
	last, ok := w.prev[s.PodUID]
	w.prev[s.PodUID] = s
	if !ok {
		return // primeira amostra do pod: só estabelece a base
	}
	dt := s.Time.Sub(last.Time).Seconds()
	if dt <= 0 {
		return
	}
	key := m.Node + "|" + m.Namespace + "|" + m.WorkloadKind + "|" + m.WorkloadName
	row, ok := w.acc[key]
	if !ok {
		row = &wire.WorkloadUsage{Node: m.Node, Namespace: m.Namespace,
			WorkloadKind: m.WorkloadKind, WorkloadName: m.WorkloadName, Labels: m.Labels}
		w.acc[key] = row
	}
	if d := s.CPUUsageCoreSeconds - last.CPUUsageCoreSeconds; d > 0 {
		row.CPUUsageCoreSeconds += d
	}
	row.MemoryWorkingSetByteSeconds += float64(s.WorkingSetBytes) * dt
	row.CPURequestMilliSeconds += int64(float64(m.CPURequestMilli) * dt)
	row.MemoryRequestByteSeconds += int64(float64(m.MemoryRequestBytes) * dt)
	row.CoverageSeconds += int64(dt)
}

// Start devolve o início da janela (o main usa no envelope do snapshot).
func (w *Window) Start() time.Time { return w.start }

// Close fecha a janela e devolve as linhas ordenadas (determinismo p/ testes e diffs).
func (w *Window) Close(_ time.Time) []wire.WorkloadUsage {
	out := make([]wire.WorkloadUsage, 0, len(w.acc))
	for _, r := range w.acc {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.WorkloadName != b.WorkloadName {
			return a.WorkloadName < b.WorkloadName
		}
		return a.Node < b.Node
	})
	return out
}
