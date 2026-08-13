// Package collect transforma objetos da kube API e do kubelet em dados do contrato wire.
package collect

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/nuvemcash/agent/wire"
)

// lbAnnotationAllowlist são os únicos prefixos de annotation repassados do Service
// LoadBalancer. O propósito é achar OCID/refs do LB no backend; annotations como
// kubectl.kubernetes.io/last-applied-configuration embutem o objeto inteiro — sobrecoleta
// que fere a promessa de read-only mínimo do agente.
var lbAnnotationAllowlist = []string{
	"service.beta.kubernetes.io/",
	"oci.oraclecloud.com/",
	"service.kubernetes.io/",
}

// filterAnnotations devolve um novo map só com as chaves permitidas pela allowlist —
// nunca muta o map de origem (é o mesmo map do objeto lido do informer/lister).
func filterAnnotations(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		for _, prefix := range lbAnnotationAllowlist {
			if strings.HasPrefix(k, prefix) {
				if out == nil {
					out = make(map[string]string, len(in))
				}
				out[k] = v
				break
			}
		}
	}
	return out
}

// NodeInventory materializa o inventário de nós. providerID vai CRU — normalização
// por provedor (ex.: prefixo oci://) é responsabilidade do backend (spec, decisão 1).
func NodeInventory(nodes []*corev1.Node) []wire.Node {
	out := make([]wire.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, wire.Node{
			Name:                   n.Name,
			ProviderID:             n.Spec.ProviderID,
			Labels:                 n.Labels,
			CPUCapacityMilli:       n.Status.Capacity.Cpu().MilliValue(),
			CPUAllocatableMilli:    n.Status.Allocatable.Cpu().MilliValue(),
			MemoryCapacityBytes:    n.Status.Capacity.Memory().Value(),
			MemoryAllocatableBytes: n.Status.Allocatable.Memory().Value(),
		})
	}
	return out
}

// PVCInventory materializa PVCs Bound. VolumeHandle vem do PV CSI correspondente —
// em OKE é o OCID do block volume, a chave da atribuição direta (spec, decisão 10).
func PVCInventory(pvcs []*corev1.PersistentVolumeClaim, pvByName map[string]*corev1.PersistentVolume) []wire.PVC {
	out := make([]wire.PVC, 0, len(pvcs))
	for _, p := range pvcs {
		if p.Status.Phase != corev1.ClaimBound {
			continue
		}
		handle := ""
		if pv, ok := pvByName[p.Spec.VolumeName]; ok && pv.Spec.CSI != nil {
			handle = pv.Spec.CSI.VolumeHandle
		}
		capQty := p.Status.Capacity[corev1.ResourceStorage]
		out = append(out, wire.PVC{
			Namespace:     p.Namespace,
			Name:          p.Name,
			VolumeHandle:  handle,
			CapacityBytes: capQty.Value(),
		})
	}
	return out
}

// LBInventory materializa Services type=LoadBalancer (correlação com a fatura no backend).
func LBInventory(svcs []*corev1.Service) []wire.LoadBalancer {
	out := make([]wire.LoadBalancer, 0)
	for _, s := range svcs {
		if s.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		lb := wire.LoadBalancer{Namespace: s.Namespace, Name: s.Name, Annotations: filterAnnotations(s.Annotations)}
		for _, ing := range s.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				lb.Ingress = append(lb.Ingress, ing.IP)
			} else if ing.Hostname != "" {
				lb.Ingress = append(lb.Ingress, ing.Hostname)
			}
		}
		out = append(out, lb)
	}
	return out
}
