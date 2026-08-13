package collect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeInventory(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"pool": "a"}},
		Spec:       corev1.NodeSpec{ProviderID: "oci://ocid1.instance.oc1..abc"},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3900m"),
				corev1.ResourceMemory: resource.MustParse("15Gi"),
			},
		},
	}
	out := NodeInventory([]*corev1.Node{n})
	if len(out) != 1 || out[0].CPUCapacityMilli != 4000 || out[0].CPUAllocatableMilli != 3900 {
		t.Fatalf("inventário errado: %+v", out)
	}
	// providerID é repassado CRU — normalização (ex.: prefixo oci://) é do backend.
	if out[0].ProviderID != "oci://ocid1.instance.oc1..abc" {
		t.Fatalf("providerID não deve ser normalizado no agente: %q", out[0].ProviderID)
	}
}

func TestPVCInventory_SoBound(t *testing.T) {
	bound := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "data"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")},
		},
	}
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "wait"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	bound.Spec.VolumeName = "pv-1"
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "blockvolume.csi.oraclecloud.com",
				VolumeHandle: "ocid1.volume.oc1..v1"},
		}},
	}
	out := PVCInventory([]*corev1.PersistentVolumeClaim{bound, pending},
		map[string]*corev1.PersistentVolume{"pv-1": pv})
	if len(out) != 1 || out[0].Name != "data" || out[0].CapacityBytes != 50*1024*1024*1024 {
		t.Fatalf("PVC inventory errado: %+v", out)
	}
	// VolumeHandle vem do PV CSI (OCID em OKE) — base da atribuição direta da Fase 4.
	if out[0].VolumeHandle != "ocid1.volume.oc1..v1" {
		t.Fatalf("volumeHandle errado: %q", out[0].VolumeHandle)
	}
}

func TestLBInventory_SoLoadBalancer(t *testing.T) {
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-lb"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: "1.2.3.4"}},
		}},
	}
	clusterIP := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}
	out := LBInventory([]*corev1.Service{lb, clusterIP})
	if len(out) != 1 || out[0].Ingress[0] != "1.2.3.4" {
		t.Fatalf("LB inventory errado: %+v", out)
	}
}
