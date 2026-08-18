package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// O liveness precisa passar desde o primeiro instante, senão o kubelet reinicia o agente
// antes de ele terminar de sincronizar os caches; o readiness é que espera a sincronização.
func TestProbesSeparamVivoDePronto(t *testing.T) {
	var ready atomic.Bool
	mux := probeMux(&ready)

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
