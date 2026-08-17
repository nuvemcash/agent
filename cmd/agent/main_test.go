package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	out, err := trimReplicaSetTemplate(rs)
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

// A factory aplica o transform a TODOS os informers — os outros tipos passam intactos.
func TestTrimReplicaSetTemplateIgnoraOutrosTipos(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "prod"}}
	out, err := trimReplicaSetTemplate(pod)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if out != any(pod) {
		t.Errorf("pod foi alterado pelo transform: %+v", out)
	}
}

func ptr[T any](v T) *T { return &v }
