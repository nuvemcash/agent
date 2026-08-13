package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// PodSample é uma amostra pod-level da Summary API (cumulativo de CPU + working set).
type PodSample struct {
	Namespace           string
	PodName             string
	PodUID              string
	Time                time.Time
	CPUUsageCoreSeconds float64 // cumulativo desde o start do pod (Δ é do agregador)
	WorkingSetBytes     int64
}

// ScrapeNode coleta o /stats/summary de UM nó via apiserver proxy (RBAC: nodes/proxy get).
// Não fala com o kubelet diretamente — funciona em qualquer managed cluster.
func ScrapeNode(ctx context.Context, client kubernetes.Interface, node string) ([]PodSample, error) {
	// nodes/proxy é uma chamada long-running para o apiserver — o request-timeout padrão
	// não se aplica a ela. Sem um deadline próprio aqui, um kubelet mudo trava o select
	// loop inteiro do agente. NÃO configurar Timeout global no rest.Config: isso mataria
	// os watches dos informers, que compartilham o mesmo client.
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := client.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node).
		SubResource("proxy").Suffix("stats/summary").
		DoRaw(sctx)
	if err != nil {
		return nil, fmt.Errorf("summary do nó %s: %w", node, err)
	}
	return parseSummary(raw)
}

func parseSummary(raw []byte) ([]PodSample, error) {
	var sum stats.Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return nil, fmt.Errorf("parse summary: %w", err)
	}
	out := make([]PodSample, 0, len(sum.Pods))
	for _, p := range sum.Pods {
		// Campos são ponteiros no statsapi — pod sem stats ainda (recém-criado) é pulado.
		if p.CPU == nil || p.CPU.UsageCoreNanoSeconds == nil || p.Memory == nil || p.Memory.WorkingSetBytes == nil {
			continue
		}
		out = append(out, PodSample{
			Namespace:           p.PodRef.Namespace,
			PodName:             p.PodRef.Name,
			PodUID:              p.PodRef.UID,
			Time:                p.CPU.Time.Time,
			CPUUsageCoreSeconds: float64(*p.CPU.UsageCoreNanoSeconds) / 1e9,
			WorkingSetBytes:     int64(*p.Memory.WorkingSetBytes), //nolint:gosec // working set < 2^63
		})
	}
	return out, nil
}
