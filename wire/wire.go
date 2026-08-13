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
}

// Node é o inventário de um nó no fim da janela.
type Node struct {
	Name                   string            `json:"name"`
	ProviderID             string            `json:"providerId,omitempty"` // OCID puro em OKE; formatos variam por cloud
	Labels                 map[string]string `json:"labels,omitempty"`
	CPUCapacityMilli       int64             `json:"cpuCapacityMilli"`
	CPUAllocatableMilli    int64             `json:"cpuAllocatableMilli"`
	MemoryCapacityBytes    int64             `json:"memoryCapacityBytes"`
	MemoryAllocatableBytes int64             `json:"memoryAllocatableBytes"`
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
	CoverageSeconds             int64   `json:"coverageSeconds"`

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
