// Package wire define o contrato de fio agente→nuvem.cash. A api do nuvem.cash importa
// estes tipos na Fase 3 (repo público). Mudanças aqui são mudanças de CONTRATO:
// versionadas por SchemaVersion e nunca quebradas silenciosamente.
package wire

import "time"

// SchemaVersion corrente do envelope.
const SchemaVersion = 1

// Path é o caminho do ingest no backend (host vem da config do agente).
const Path = "/ingest/k8s/v1/snapshots"

// Snapshot é o envelope de uma janela de agregação (~5 min).
type Snapshot struct {
	SchemaVersion int       `json:"schemaVersion"`
	AgentVersion  string    `json:"agentVersion"`
	ClusterUID    string    `json:"clusterUid"` // UID do namespace kube-system (fingerprint)
	WindowStart   time.Time `json:"windowStart"`
	WindowEnd     time.Time `json:"windowEnd"`

	Nodes         []Node          `json:"nodes"`
	Usage         []WorkloadUsage `json:"usage"`
	PVCs          []PVC           `json:"pvcs,omitempty"`
	LoadBalancers []LoadBalancer  `json:"loadBalancers,omitempty"`

	// Agent é a saúde do PRÓPRIO agente. Viaja no contrato de fio de propósito: o
	// /metrics do agente serve ao cliente, não a nós — é o cluster dele, e nada nosso
	// raspa aquele endpoint. Sem este bloco, uma janela descartada por buffer cheio some
	// com um log no cluster do cliente e nunca chega ao nosso lado.
	//
	// Ponteiro e omitempty: agente antigo não manda, e ausência tem de ser distinguível
	// de zero — "nenhuma janela descartada" e "não sei se descartou" não são a mesma coisa.
	Agent *AgentHealth `json:"agent,omitempty"`
}

// AgentHealth é a autotelemetria do agente na janela. ADITIVO no SchemaVersion 1: o ingest
// do nuvem.cash ignora campo desconhecido, então um backend antigo simplesmente não o lê.
type AgentHealth struct {
	// DroppedWindows é o acumulado de janelas perdidas desde a partida do processo
	// (buffer cheio ou falha de serialização). Acumulado, não por janela: reinício do pod
	// zera, e é justamente o reinício que se quer ver no gráfico.
	DroppedWindows int `json:"droppedWindows"`
	// BufferedWindows e BufferedBytes são a ocupação da fila NO MOMENTO em que a janela foi
	// montada — ou seja, ANTES de ela própria entrar. Fila crescendo é o sinal precoce de
	// ingest inacessível ou rejeitando.
	BufferedWindows int `json:"bufferedWindows"`
	BufferedBytes   int `json:"bufferedBytes"`
	// ScrapeRoundMillis é a duração do último ciclo de scrape de nós. Encostando no
	// ScrapeInterval, o ticker começa a descartar ticks e a cobertura cai — que empurra
	// custo para "não monitorado" e mexe na conta do cliente.
	ScrapeRoundMillis int64 `json:"scrapeRoundMillis"`
}

// Node é o inventário de um nó no fim da janela.
type Node struct {
	Name       string `json:"name"`
	ProviderID string `json:"providerId,omitempty"` // OCID puro em OKE; formatos variam por cloud
	// NodePool identifica o grupo de nós (node pool/nodegroup/agentpool). Resolvido pelo
	// agente numa allowlist de labels e annotations conhecidos — vazio quando o cluster não
	// tem o conceito. O backend agrupa a capacidade ociosa por este campo.
	NodePool               string            `json:"nodePool,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	CPUCapacityMilli       int64             `json:"cpuCapacityMilli"`
	CPUAllocatableMilli    int64             `json:"cpuAllocatableMilli"`
	MemoryCapacityBytes    int64             `json:"memoryCapacityBytes"`
	MemoryAllocatableBytes int64             `json:"memoryAllocatableBytes"`
	// SampledSeconds são os segundos de amostragem cobertos neste nó dentro da janela;
	// 0 = nó não monitorado (kubelet inacessível durante toda a janela) — o backend usa
	// isso para distinguir ociosidade real de período não monitorado (spec decisão 5).
	SampledSeconds int64 `json:"sampledSeconds,omitempty"`
}

// WorkloadUsage agrega o uso de UM workload em UM nó dentro da janela.
// Integrais em recurso·segundo — o rateio (max(request, uso)/hora) é do backend.
type WorkloadUsage struct {
	Node         string `json:"node"`
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"` // Deployment|StatefulSet|DaemonSet|CronJob|Job|Pod
	WorkloadName string `json:"workloadName"`

	CPURequestMilliSeconds      int64   `json:"cpuRequestMilliSeconds"`      // Σ requests(milicore)×dt
	CPUUsageCoreSeconds         float64 `json:"cpuUsageCoreSeconds"`         // Δ do cumulativo do kubelet
	MemoryRequestByteSeconds    int64   `json:"memoryRequestByteSeconds"`    // Σ requests(bytes)×dt
	MemoryWorkingSetByteSeconds float64 `json:"memoryWorkingSetByteSeconds"` // Σ workingSet(bytes)×dt
	// CoverageSeconds conta a partir da 2ª amostra do pod (a 1ª só estabelece a base do
	// Δ, sem intervalo); pode exceder a duração nominal da janela quando há gap de scrape
	// (o intervalo dt observado é maior que o ScrapeInterval configurado).
	CoverageSeconds int64 `json:"coverageSeconds"`

	Labels map[string]string `json:"labels,omitempty"` // labels do pod no momento da amostra (fonte padrão de classificação — GKE/SCAD usam labels de pod); hashes de template são removidos pelo agente
}

// PVC é o inventário de um PersistentVolumeClaim Bound (atribuição direta no backend).
type PVC struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	VolumeHandle  string `json:"volumeHandle,omitempty"` // CSI: OCID em OKE
	CapacityBytes int64  `json:"capacityBytes"`
}

// LoadBalancer é um Service type=LoadBalancer (correlação com a fatura no backend).
type LoadBalancer struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Ingress     []string          `json:"ingress,omitempty"`
}
