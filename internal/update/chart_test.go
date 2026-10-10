package update_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

// O teto de permissões (ADR 0017) é uma promessa ao cliente. Estes testes renderizam o
// chart de verdade e conferem a promessa nos objetos, não nos templates.

type rendered struct {
	objs []runtime.Object
	raw  []map[string]any // objetos que o scheme do client-go não conhece (PodMonitor...)
}

func render(t *testing.T, vals map[string]any, apiVersions ...string) rendered {
	t.Helper()
	cfg := helmConfig(t, nil)
	in := action.NewInstall(cfg)
	in.ReleaseName, in.Namespace, in.DryRunStrategy = releaseName, namespace, action.DryRunClient
	in.KubeVersion, in.APIVersions = &cfg.Capabilities.KubeVersion, apiVersions
	base := map[string]any{"connection": map[string]any{"token": token}}
	for k, v := range vals {
		base[k] = v
	}
	r, err := in.Run(loadChart(t, "0.1.0"), base)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var out rendered
	for _, doc := range strings.Split(r.(*rspb.Release).Manifest, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(doc), nil, nil)
		if err != nil {
			var m map[string]any
			if yerr := yaml.Unmarshal([]byte(doc), &m); yerr != nil {
				t.Fatalf("manifesto inválido: %v\n%s", yerr, doc)
			}
			out.raw = append(out.raw, m)
			continue
		}
		out.objs = append(out.objs, obj)
	}
	return out
}

func find[T runtime.Object](r rendered, name string) (T, bool) {
	for _, o := range r.objs {
		if v, ok := o.(T); ok {
			if m, ok := any(v).(interface{ GetName() string }); ok && m.GetName() == name {
				return v, true
			}
		}
	}
	var zero T
	return zero, false
}

const updater = releaseName + "-updater"

func TestChartRendersUpdaterCronJob(t *testing.T) {
	r := render(t, nil)
	cj, ok := find[*batchv1.CronJob](r, updater)
	if !ok {
		t.Fatal("CronJob do updater ausente com o default autoUpgrade.enabled=true")
	}
	dep, _ := find[*appsv1.Deployment](r, releaseName)
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	if cj.Spec.Schedule != "0 * * * *" || cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Fatalf("de hora em hora e sem concorrência: %q %q", cj.Spec.Schedule, cj.Spec.ConcurrencyPolicy)
	}
	if pod.ServiceAccountName != updater || pod.ServiceAccountName == dep.Spec.Template.Spec.ServiceAccountName {
		t.Fatalf("o updater precisa de identidade própria: %q", pod.ServiceAccountName)
	}
	if bl := cj.Spec.JobTemplate.Spec.BackoffLimit; bl == nil || *bl != 0 {
		t.Fatal("sem retry agressivo: backoffLimit 0")
	}
	if d := cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds; d == nil || *d != 900 {
		t.Fatal("activeDeadlineSeconds tem de casar com runDeadline (15min) do cmd/agent")
	}
	c := pod.Containers[0]
	if !slices.Equal(c.Args, []string{"update"}) || c.Image != dep.Spec.Template.Spec.Containers[0].Image {
		t.Fatalf("updater roda o subcomando update na MESMA imagem do agente: %v %s", c.Args, c.Image)
	}
	if mu := dep.Spec.Strategy.RollingUpdate; mu == nil || mu.MaxUnavailable == nil || mu.MaxUnavailable.IntValue() != 0 {
		t.Fatal("Deployment do coletor precisa de maxUnavailable: 0 explícito")
	}
}

func TestChartWithoutAutoUpgradeRendersNoUpdater(t *testing.T) {
	r := render(t, map[string]any{"autoUpgrade": map[string]any{"enabled": false}})
	for _, o := range r.objs {
		if m, ok := o.(interface{ GetName() string }); ok && strings.HasSuffix(m.GetName(), "-updater") {
			t.Fatalf("autoUpgrade.enabled=false ainda renderiza %T %s", o, m.GetName())
		}
	}
}

// Com o updater desligado não há CronJob para relatar nada: quem conta à api é o coletor,
// e para isso o chart lhe diz se a atualização automática está ligada.
func TestChartTellsCollectorWhetherAutoUpgradeIsEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		r := render(t, map[string]any{"autoUpgrade": map[string]any{"enabled": enabled}})
		dep, ok := find[*appsv1.Deployment](r, releaseName)
		if !ok {
			t.Fatal("Deployment do coletor ausente")
		}
		got := ""
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED" {
				got = e.Value
			}
		}
		if want := strconv.FormatBool(enabled); got != want {
			t.Fatalf("autoUpgrade.enabled=%v: env do coletor = %q, quer %q", enabled, got, want)
		}
	}
}

// O coletor e o updater têm de concordar sobre "ligado": a env deriva da mesma truthiness
// do `if $au.enabled` do updater.yaml. Com o schema o chart rejeita string; o que sobra
// vivo é null (Helm apaga a chave, sem default: desligado), false e true.
func TestChartCollectorEnvAgreesWithUpdaterRender(t *testing.T) {
	cases := map[string]struct {
		au      any
		enabled bool
	}{
		"true":    {au: map[string]any{"enabled": true}, enabled: true},
		"false":   {au: map[string]any{"enabled": false}, enabled: false},
		"null":    {au: map[string]any{"enabled": nil}, enabled: false}, // null apaga a chave: sem updater, e a env diz false
		"ausente": {au: map[string]any{}, enabled: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := render(t, map[string]any{"autoUpgrade": c.au})
			dep, ok := find[*appsv1.Deployment](r, releaseName)
			if !ok {
				t.Fatal("Deployment do coletor ausente")
			}
			got := ""
			for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "NUVEMCASH_AGENT_AUTO_UPGRADE_ENABLED" {
					got = e.Value
				}
			}
			_, hasUpdater := find[*batchv1.CronJob](r, releaseName+"-updater")
			if want := strconv.FormatBool(c.enabled); got != want || hasUpdater != c.enabled {
				t.Fatalf("env = %q, updater renderizado = %v; quer %q e %v", got, hasUpdater, want, c.enabled)
			}
		})
	}
	t.Run("string é rejeitada pelo schema", func(t *testing.T) {
		cfg := helmConfig(t, nil)
		in := action.NewInstall(cfg)
		in.ReleaseName, in.Namespace, in.DryRunStrategy = releaseName, namespace, action.DryRunClient
		vals := map[string]any{"connection": map[string]any{"token": token}, "autoUpgrade": map[string]any{"enabled": "false"}}
		if _, err := in.Run(loadChart(t, "0.1.0"), vals); err == nil {
			t.Fatal(`autoUpgrade.enabled: "false" (string) deveria ser rejeitado pelo schema`)
		}
	})
}

func rulesOf(t *testing.T, r rendered) (collector, ceiling, role []rbacv1.PolicyRule) {
	t.Helper()
	cr, ok1 := find[*rbacv1.ClusterRole](r, releaseName)
	ce, ok2 := find[*rbacv1.ClusterRole](r, updater)
	ro, ok3 := find[*rbacv1.Role](r, updater)
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("ClusterRole do coletor, teto ou Role do updater ausente")
	}
	return cr.Rules, ce.Rules, ro.Rules
}

// allows diz se as regras concedem verb em group/resource (name vazio = qualquer nome).
func allows(rules []rbacv1.PolicyRule, group, resource, verb, name string) bool {
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) &&
			slices.Contains(r.Verbs, verb) &&
			(len(r.ResourceNames) == 0 || (name != "" && slices.Contains(r.ResourceNames, name))) {
			return true
		}
	}
	return false
}

func TestUpdaterRBACNeverEscalates(t *testing.T) {
	_, ceiling, role := rulesOf(t, render(t, nil))
	for _, r := range append(slices.Clone(ceiling), role...) {
		for _, bad := range []string{"*", "escalate", "bind", "impersonate"} {
			if slices.Contains(r.Verbs, bad) || slices.Contains(r.Resources, bad) || slices.Contains(r.APIGroups, bad) {
				t.Fatalf("regra proibida no updater: %+v", r)
			}
		}
	}
	for _, r := range ceiling {
		if slices.Contains(r.Resources, "secrets") {
			t.Fatalf("o teto não lê secrets: %+v", r)
		}
		writes := slices.ContainsFunc(r.Verbs, func(v string) bool { return v != "get" && v != "list" && v != "watch" })
		if !writes {
			continue
		}
		if !slices.Equal(r.APIGroups, []string{"rbac.authorization.k8s.io"}) || len(r.ResourceNames) == 0 ||
			!slices.Equal(r.Verbs, []string{"update", "patch"}) {
			t.Fatalf("escrita no teto só update/patch nas roles do próprio agente, por nome: %+v", r)
		}
	}
}

// O apiserver só deixa o updater gravar a ClusterRole do coletor se ele próprio tiver
// cada permissão dela. Coletor fora do teto = toda atualização recusada.
func TestCeilingCoversCollector(t *testing.T) {
	collector, ceiling, _ := rulesOf(t, render(t, nil))
	for _, r := range collector {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					if !allows(ceiling, g, res, v, "") {
						t.Errorf("teto não cobre o coletor: %s %s/%s", v, g, res)
					}
				}
			}
		}
	}
}

// Tudo o que o chart renderiza o updater consegue regravar: os objetos do namespace pela
// Role e os de cluster pelo update/patch restrito por nome.
func TestUpdaterCanRewriteEveryRenderedObject(t *testing.T) {
	r := render(t, nil, "monitoring.coreos.com/v1/PodMonitor", "monitoring.coreos.com/v1/PrometheusRule")
	_, ceiling, role := rulesOf(t, r)
	type obj struct{ group, kind, name string }
	var all []obj
	for _, o := range r.objs {
		gvk := o.GetObjectKind().GroupVersionKind()
		all = append(all, obj{gvk.Group, gvk.Kind, o.(interface{ GetName() string }).GetName()})
	}
	for _, m := range r.raw {
		group, _, _ := strings.Cut(m["apiVersion"].(string), "/")
		all = append(all, obj{group, m["kind"].(string), m["metadata"].(map[string]any)["name"].(string)})
	}
	if len(r.raw) != 2 {
		t.Fatalf("esperava PodMonitor e PrometheusRule no render: %d", len(r.raw))
	}
	for _, o := range all {
		res := strings.ToLower(o.kind) + "s"
		rules := role
		if o.kind == "ClusterRole" || o.kind == "ClusterRoleBinding" {
			rules = ceiling
		}
		for _, v := range []string{"get", "update", "patch"} {
			if !allows(rules, o.group, res, v, o.name) {
				t.Errorf("updater não consegue %s %s/%s %s", v, o.group, res, o.name)
			}
		}
	}
}

// A assinatura do chart só cobre a imagem se o digest estiver dentro dele: com
// image.digest a referência sai repo@digest, no coletor e no updater (que usam a MESMA).
// image.tag explícito do usuário vence o digest; sem nenhum dos dois, vale o appVersion.
func TestChartImageReference(t *testing.T) {
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const repo = "ghcr.io/nuvemcash/agent"
	cases := map[string]struct {
		image map[string]any
		want  string
	}{
		"digest":        {image: map[string]any{"digest": digest}, want: repo + "@" + digest},
		"tag explícita": {image: map[string]any{"digest": digest, "tag": "custom"}, want: repo + ":custom"},
		"sem digest":    {image: map[string]any{}, want: repo + ":0.1.0"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := render(t, map[string]any{"image": c.image})
			dep, _ := find[*appsv1.Deployment](r, releaseName)
			cj, _ := find[*batchv1.CronJob](r, updater)
			got := []string{
				dep.Spec.Template.Spec.Containers[0].Image,
				cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image,
			}
			if got[0] != c.want || got[1] != c.want {
				t.Fatalf("imagem do Deployment e do updater = %q; quer %q", got, c.want)
			}
		})
	}
}
