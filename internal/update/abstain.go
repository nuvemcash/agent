package update

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nuvemcash/agent/wire"
)

// DefaultImageRepository é o repositório da imagem que o chart instala por padrão. Outro
// valor é registry espelhado: o digest do alvo pode não existir lá.
const DefaultImageRepository = "ghcr.io/nuvemcash/agent"

// Prefixos de posse: quem gerencia a release por GitOps carimba os recursos com eles, e
// um upgrade nosso brigaria com o reconciliador a cada hora.
const (
	fluxPrefix = "helm.toolkit.fluxcd.io/"
	argoPrefix = "argocd.argoproj.io/"
)

// abstention decide, só com leituras no cluster, se o updater deve deixar a release em
// paz. reason vazio significa "pode atualizar". version é a do agente instalado, para o
// relato; vazia quando não deu para saber (o relato é então omitido).
//
// Roda antes de qualquer leitura da release do Helm: o Argo aplica os manifestos
// renderizados e não deixa release no cluster, e a recuperação de release presa escreve.
func (u *Updater) abstention(ctx context.Context) (reason, version string, err error) {
	dep, err := u.Kube.AppsV1().Deployments(u.Namespace).Get(ctx, u.Release, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", fmt.Errorf("read deployment %s: %w", u.Release, err)
	}
	metas := []metav1.ObjectMeta{dep.ObjectMeta}
	cj, err := u.Kube.BatchV1().CronJobs(u.Namespace).Get(ctx, u.Release+"-updater", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", fmt.Errorf("read cronjob %s-updater: %w", u.Release, err)
	}
	metas = append(metas, cj.ObjectMeta)

	repo, tag := "", ""
	if cs := dep.Spec.Template.Spec.Containers; len(cs) > 0 {
		repo, tag = splitImage(cs[0].Image)
	}
	switch {
	case hasMarker(metas, fluxPrefix):
		reason = wire.ReasonGitOpsFlux
	case hasMarker(metas, argoPrefix):
		reason = wire.ReasonGitOpsArgo
	case repo != "" && repo != DefaultImageRepository:
		reason = wire.ReasonMirroredRegistry
	default:
		return "", "", nil
	}
	// Versão: a do chart da release, se houver; senão a tag da imagem (Argo sem release).
	if rel, relErr := asV1(u.Helm.Releases.Deployed(u.Release)); relErr == nil {
		return reason, rel.Chart.Metadata.Version, nil
	}
	return reason, tag, nil
}

// reportAbstention relata o motivo à api; sem versão semver não há o que relatar.
func (u *Updater) reportAbstention(ctx context.Context, reason, version string) error {
	slog.Info("release managed elsewhere, not updating", "reason", reason, "version", version)
	if version == "" {
		return nil
	}
	return u.API.Report(ctx, wire.AgentUpdateReport{Version: version, Outcome: wire.OutcomeAbstained, Reason: reason})
}

func hasMarker(metas []metav1.ObjectMeta, prefix string) bool {
	for _, m := range metas {
		for _, kv := range []map[string]string{m.Labels, m.Annotations} {
			for k := range kv {
				if strings.HasPrefix(k, prefix) {
					return true
				}
			}
		}
	}
	return false
}

// splitImage separa "reg[:porta]/repo[:tag][@digest]" em repositório e tag.
func splitImage(image string) (repo, tag string) {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, ""
}
