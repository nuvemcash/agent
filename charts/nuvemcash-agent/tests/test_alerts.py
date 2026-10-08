#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = ["PyYAML==6.0.3", "jsonschema==4.26.0"]
# ///
"""Valida os manifestos finais e as regras portáveis do chart do agente."""
import pathlib
import subprocess
import tempfile
import yaml

CHART = pathlib.Path(__file__).resolve().parents[1]
POD = "monitoring.coreos.com/v1/PodMonitor"
RULE = "monitoring.coreos.com/v1/PrometheusRule"


def render(apis=(), release="agent-a", namespace="local-a", overrides=(), upgrade=False):
    cmd = ["helm", "template", release, str(CHART), "--namespace", namespace,
           "--set", "connection.existingSecret=agent-token", "--set", "monitoring.cluster=cluster-a"]
    if upgrade:
        cmd.append("--is-upgrade")
    for api in apis:
        cmd.extend(["--api-versions", api])
    for value in overrides:
        cmd.extend(["--set", value])
    return [d for d in yaml.safe_load_all(subprocess.check_output(cmd, text=True)) if d]


for apis in [(), (POD,), (RULE,), (POD, RULE)]:
    docs = render(apis)
    kinds = [d["kind"] for d in docs]
    assert kinds.count("PodMonitor") == (POD in apis), kinds
    assert kinds.count("PrometheusRule") == (RULE in apis), kinds
    dep = next(d for d in docs if d["kind"] == "Deployment")
    env = dep["spec"]["template"]["spec"]["containers"][0]["env"]
    assert {v["name"]: v.get("value") for v in env}["NUVEMCASH_AGENT_METRICS_ENABLED"] == "true"
print("Matriz de CRDs e métricas padrão: OK")

# Token-only continua instalável com qualquer combinação de CRDs.
for apis in [(), (POD,), (RULE,), (POD, RULE)]:
    docs = render(apis, overrides=("monitoring.cluster=",), upgrade=True)
    for doc in docs:
        if doc["kind"] == "PodMonitor":
            relabelings = doc["spec"]["podMetricsEndpoints"][0]["relabelings"]
            assert "cluster" not in {r["targetLabel"] for r in relabelings}
        if doc["kind"] == "PrometheusRule":
            for rule in doc["spec"]["groups"][0]["rules"]:
                assert "cluster" not in rule["labels"]
                assert 'cluster!=""' in rule["expr"] and '"cluster", "local"' in rule["expr"]

for option, missing in [("monitoring.alerts.enabled=false", "PrometheusRule"),
                        ("monitoring.podMonitor.enabled=false", "PodMonitor")]:
    docs = render((POD, RULE), overrides=(option,))
    kinds = [d["kind"] for d in docs]
    assert missing not in kinds
    assert ({"PodMonitor", "PrometheusRule"} - {missing}).pop() in kinds
assert not {"PodMonitor", "PrometheusRule"}.intersection(d["kind"] for d in render((POD, RULE), overrides=("monitoring.metrics.enabled=false",)))

custom = render((POD, RULE), overrides=("monitoring.podMonitor.labels.team=local", "monitoring.alerts.labels.team=local",
    "monitoring.podMonitor.selector.app=extra", "podLabels.app=extra", "monitoring.alerts.additionalLabels.owner=ops",
    "monitoring.alerts.additionalLabels.release=invalid", "monitoring.alerts.failureSeconds=120",
    "monitoring.alerts.lossWindowSeconds=90", "monitoring.alerts.notificationMode=controlled"))
custom_pod_labels = next(d for d in custom if d["kind"] == "Deployment")["spec"]["template"]["metadata"]["labels"]
for doc in custom:
    if doc["kind"] in ("PodMonitor", "PrometheusRule"):
        assert doc["metadata"]["namespace"] == "local-a"
        assert doc["metadata"]["labels"]["team"] == "local"
    if doc["kind"] == "PodMonitor":
        assert doc["spec"]["namespaceSelector"] == {"matchNames": ["local-a"]}
        assert all(custom_pod_labels.get(k) == v for k, v in doc["spec"]["selector"]["matchLabels"].items())
        assert doc["spec"]["selector"]["matchLabels"] == {"app": "extra", "app.kubernetes.io/instance": "agent-a", "app.kubernetes.io/name": "nuvemcash-agent"}
    if doc["kind"] == "PrometheusRule":
        for rule in doc["spec"]["groups"][0]["rules"]:
            assert rule["labels"]["owner"] == "ops" and rule["labels"]["release"] == "agent-a"
            assert rule["labels"]["notification_mode"] == "controlled"
            assert '{{ $labels.release }}' in rule["annotations"]["evidence"]
        assert ">= 120" in doc["spec"]["groups"][0]["rules"][0]["expr"]
        assert "< 90" in doc["spec"]["groups"][0]["rules"][2]["expr"]

# O selector imutável do Deployment permanece igual ao chart já publicado.
for release, namespace in [("agent-a", "local-a"), ("agent-b", "local-a"), ("agent-a", "local-b")]:
    docs = render((POD, RULE), release, namespace)
    dep = next(d for d in docs if d["kind"] == "Deployment")
    assert dep["spec"]["selector"] == {"matchLabels": {"app.kubernetes.io/name": "nuvemcash-agent"}}
    assert dep["spec"]["template"]["metadata"]["labels"]["app.kubernetes.io/instance"] == release
    for doc in docs:
        if doc["kind"] in ("PodMonitor", "PrometheusRule"):
            assert doc["metadata"]["name"].startswith(release+"-")
            assert doc["metadata"]["namespace"] == namespace
        if doc["kind"] == "PrometheusRule":
            for rule in doc["spec"]["groups"][0]["rules"]:
                assert f'namespace="{namespace}"' in rule["expr"] and f'release="{release}"' in rule["expr"]
print("Configuração, identidade, isolamento e compatibilidade do selector: OK")

import json
import os
import urllib.request
from jsonschema import Draft7Validator

# Schemas oficiais das CRDs, fixados na mesma versão para renderização reproduzível.
for kind, filename in [("PodMonitor", "podmonitors"), ("PrometheusRule", "prometheusrules")]:
    url = f"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.85.0/example/prometheus-operator-crd/monitoring.coreos.com_{filename}.yaml"
    with urllib.request.urlopen(url, timeout=30) as response:
        crd = yaml.safe_load(response)
    schema = next(v["schema"]["openAPIV3Schema"] for v in crd["spec"]["versions"] if v["name"] == "v1")
    validator = Draft7Validator(schema)
    for doc in custom + render((POD, RULE)) + render((POD, RULE), overrides=("monitoring.cluster=",)):
        if doc["kind"] == kind:
            validator.validate(doc)
print("Schemas upstream PodMonitor/PrometheusRule: OK")

promtool = os.environ.get("PROMTOOL", "promtool")
version = subprocess.check_output([promtool, "--version"], text=True)
assert "version 3.15.0" in version, f"Use promtool 3.15.0, recebido: {version}"

identity = {"cluster": "cluster-a", "namespace": "local-a", "release": "agent-a", "environment": "local", "component": "agent"}


def series(metric, values, **extra):
    labels = identity | extra
    labels = {k: v for k, v in labels.items() if v is not None}
    selector = ",".join(f"{k}={json.dumps(v)}" for k, v in sorted(labels.items()))
    return {"series": f"nuvemcash_agent_{metric}{{{selector}}}", "values": values}


def alert_sample(name, condition, **extra):
    labels = identity | {"__name__": "ALERTS", "alertname": name, "alertstate": "firing", "product": "nuvemcash",
                         "notification_mode": "active", "condition": condition, "severity": "warning"} | extra
    selector = ",".join(f"{k}={json.dumps(v)}" for k, v in sorted(labels.items()))
    return {"labels": "{"+selector+"}", "value": 1}


def assertion(at, name, samples=()):
    return {"expr": f'ALERTS{{alertname="{name}",alertstate="firing"}}', "eval_time": at, "exp_samples": list(samples)}


collection = "NuvemcashAgentCollectionFailed"
ship = "NuvemcashAgentShipFailed"
loss = "NuvemcashAgentDataLoss"
# Valores de entrada representam estados públicos, não reimplementam os predicados.
tests = []
for name, metric, condition in [(collection, "collection_failure_since_timestamp_seconds", "collection_failed"),
                                (ship, "ship_failure_since_timestamp_seconds", "ship_failed")]:
    expected = alert_sample(name, condition)
    tests.extend([
        {"name": f"{condition}: dez minutos, recuperação e réplicas",
         "input_series": [series(metric, "0 60+0x10 0+0x8", instance="pod-1"), series(metric, "0 60+0x10 0+0x8", instance="pod-2")],
         "promql_expr_test": [assertion("10m", name), assertion("11m", name, [expected]), assertion("12m", name)]},
        {"name": f"{condition}: retry recuperado e ociosidade",
         "input_series": [series(metric, "0 60+0x3 0+0x15")],
         "promql_expr_test": [assertion("4m", name), assertion("11m", name), assertion("16m", name)]},
        {"name": f"{condition}: outra obrigação não herda a falha recuperada",
         "input_series": [series(metric, "0 60+0x4 360+0x15")],
         "promql_expr_test": [assertion("11m", name), assertion("16m", name, [expected])]},
        {"name": f"{condition}: isolamento exato de instalação",
         "input_series": [series(metric, "0+0x20"), series(metric, "60+0x20", cluster="cluster-b"),
                          series(metric, "60+0x20", namespace="local-b"), series(metric, "60+0x20", release="agent-b"),
                          series(metric, "60+0x20", component="api"), series(metric, "60+0x20", environment="production")],
         "promql_expr_test": [assertion("11m", name), assertion("16m", name)]},
    ])

tests.extend([
    {"name": "perdas distintas, primeira observação e expiração",
     "input_series": [series("last_drop_timestamp_seconds", "0 60+0x15", reason=reason) for reason in ["buffer_windows", "buffer_bytes", "encode", "http_rejected"]],
     "promql_expr_test": [assertion("0m", loss), assertion("1m", loss, [alert_sample(loss, "data_loss", reason=reason) for reason in ["buffer_windows", "buffer_bytes", "encode", "http_rejected"]]), assertion("11m", loss)]},
    {"name": "reinício e reset não fabricam perdas históricas",
     "input_series": [series("last_drop_timestamp_seconds", "0+0x20", reason="buffer_windows"),
                      series("dropped_windows_total", "8+0x5 0+0x14", reason="buffer_windows")],
     "promql_expr_test": [assertion("5m", loss), assertion("6m", loss), assertion("11m", loss)]},
    {"name": "reset após ocorrência real encerra observação em memória",
     "input_series": [series("last_drop_timestamp_seconds", "0 60+0x4 0+0x14", reason="http_rejected"),
                      series("dropped_windows_total", "0 1+0x4 0+0x14", reason="http_rejected")],
     "promql_expr_test": [assertion("1m", loss, [alert_sample(loss, "data_loss", reason="http_rejected")]), assertion("6m", loss), assertion("11m", loss)]},
    {"name": "perda recente não é escondida por outra réplica com perda antiga",
     "input_series": [series("last_drop_timestamp_seconds", "60+0x20", reason="buffer_bytes", instance="pod-1"),
                      series("last_drop_timestamp_seconds", "0+0x9 600+0x10", reason="buffer_bytes", instance="pod-2")],
     "promql_expr_test": [assertion("11m", loss, [alert_sample(loss, "data_loss", reason="buffer_bytes")])]},
])


def check_rules(docs, cases):
    rule = next(d for d in docs if d["kind"] == "PrometheusRule")
    with tempfile.TemporaryDirectory() as folder:
        folder = pathlib.Path(folder)
        rules = folder / "rules.yaml"
        rules.write_text(yaml.safe_dump(rule["spec"], allow_unicode=True))
        subprocess.run([promtool, "check", "rules", str(rules)], check=True)
        fixture = {"rule_files": [str(rules)], "evaluation_interval": "1m", "tests": [{"interval": "1m"} | case for case in cases]}
        path = folder / "tests.yaml"
        path.write_text(yaml.safe_dump(fixture, allow_unicode=True))
        subprocess.run([promtool, "test", "rules", str(path)], check=True)


check_rules(render((RULE,)), tests)
# Sem cluster explícito: preserve o cluster do coletor e mantenha fallback estritamente local.
check_rules(render((RULE,), overrides=("monitoring.cluster=",)), [
    {"name": "cluster do coletor preservado, sem contaminação entre clusters",
     "input_series": [series("ship_failure_since_timestamp_seconds", "0+0x20"), series("ship_failure_since_timestamp_seconds", "60+0x20", cluster="cluster-b")],
     "promql_expr_test": [assertion("11m", ship, [alert_sample(ship, "ship_failed", cluster="cluster-b")])]},
    {"name": "instalação token-only local sem label de cluster",
     "input_series": [series("collection_failure_since_timestamp_seconds", "60+0x20", cluster=None)],
     "promql_expr_test": [assertion("11m", collection, [alert_sample(collection, "collection_failed", cluster="local")])]},
])
check_rules(custom, [
    {"name": "limiar personalizado e modo controlado",
     "input_series": [series("ship_failure_since_timestamp_seconds", "0 60+0x6")],
     "promql_expr_test": [assertion("2m", ship), assertion("3m", ship, [alert_sample(ship, "ship_failed", notification_mode="controlled", owner="ops")])]},
])
# Labels dinâmicos não podem ser substituídos por adicionais da instalação.
reserved_loss = {
    "name": "motivos distintos sobrevivem a additionalLabels.reason",
    "input_series": [series("last_drop_timestamp_seconds", "0 60+0x15", reason=reason) for reason in ["buffer_windows", "http_rejected"]],
    "promql_expr_test": [assertion("1m", loss, [alert_sample(loss, "data_loss", reason=reason) for reason in ["buffer_windows", "http_rejected"]])],
}
reserved_overrides = ("monitoring.alerts.additionalLabels.cluster=fixed", "monitoring.alerts.additionalLabels.reason=fixed")
check_rules(render((RULE,), overrides=("monitoring.cluster=",) + reserved_overrides), [
    {"name": "clusters distintos sobrevivem a additionalLabels.cluster",
     "input_series": [series("ship_failure_since_timestamp_seconds", "60+0x20", cluster=cluster) for cluster in ["cluster-a", "cluster-b"]],
     "promql_expr_test": [assertion("11m", ship, [alert_sample(ship, "ship_failed", cluster=cluster) for cluster in ["cluster-a", "cluster-b"]])]},
    reserved_loss,
])
check_rules(render((RULE,), overrides=reserved_overrides), [reserved_loss])
# Aspas, barra e nova linha são valores de labels, nunca sintaxe PromQL interpolada.
with tempfile.TemporaryDirectory() as folder:
    values = pathlib.Path(folder) / "escape.yaml"
    values.write_text(yaml.safe_dump({"monitoring": {"cluster": 'clu"ster\\a\nb', "environment": 'lo"cal\\x\ny'}}))
    cmd = ["helm", "template", "agent-a", str(CHART), "--namespace", "local-a", "--set", "connection.existingSecret=agent-token",
           "--api-versions", RULE, "-f", str(values)]
    check_rules(list(yaml.safe_load_all(subprocess.check_output(cmd, text=True))), [])
print("Disparo, recuperação, retries, resets, isolamento, configuração e escapes: OK")

outdated = "NuvemcashAgentOutdated"
version_labels = {"installed_version": "v0.9.0", "latest_version": "v0.10.0"}


def version_series(values, status="outdated", **extra):
    return series("version_status", values, **({"status": status} | version_labels | extra))


def version_alert(**extra):
    return alert_sample(outdated, "version_outdated", **({"severity": "info"} | version_labels | extra))


version_docs = render((RULE,))
version_rule = next(r for d in version_docs if d["kind"] == "PrometheusRule" for r in d["spec"]["groups"][0]["rules"] if r["alert"] == outdated)
assert version_rule["for"] == "72h"
assert version_rule["labels"]["severity"] == "info"
check_rules(version_docs, [
    {"name": "versão informativa respeita as 72 horas herdadas",
     "input_series": [version_series("1+0x4321")],
     "promql_expr_test": [assertion("4319m", outdated), assertion("4320m", outdated, [version_alert()])]},
])
short_version = ("monitoring.alerts.outdatedFor=2m",)
check_rules(render((RULE,), overrides=short_version), [
    {"name": "versão conhecida desatualizada dispara e atualização válida recupera",
     "input_series": [version_series("1+0x3 0+0x4"), version_series("0+0x3 1+0x4", status="updated", installed_version="v0.10.0")],
     "promql_expr_test": [assertion("1m", outdated), assertion("2m", outdated, [version_alert()]), assertion("4m", outdated)]},
    {"name": "referência desconhecida encerra aviso e reinicia a espera sem herdar referência",
     "input_series": [version_series("1+0x2 stale 1+0x4"), version_series("_ _ _ 1 stale", status="unknown", latest_version="")],
     "promql_expr_test": [assertion("2m", outdated, [version_alert()]), assertion("3m", outdated), assertion("5m", outdated), assertion("6m", outdated, [version_alert()])]},
    {"name": "api antiga e build dev permanecem desconhecidos sem aviso",
     "input_series": [version_series("1+0x8", status="unknown", latest_version=""), version_series("1+0x8", status="unknown", installed_version="", latest_version="v0.10.0")],
     "promql_expr_test": [assertion("6m", outdated)]},
    {"name": "instalação diferente não dispara nem recupera versão local",
     "input_series": [version_series("1+0x8", status="updated"), version_series("1+0x8", cluster="cluster-b"), version_series("1+0x8", namespace="local-b"), version_series("1+0x8", release="agent-b"), version_series("1+0x8", environment="production"), version_series("1+0x8", component="api")],
     "promql_expr_test": [assertion("6m", outdated)]},
    {"name": "réplicas atualizadas não ocultam pod desatualizado",
     "input_series": [version_series("1+0x8", instance="pod-1"), version_series("1+0x8", status="updated", installed_version="v0.10.0", instance="pod-2")],
     "promql_expr_test": [assertion("2m", outdated, [version_alert()])]},
])
version_reserved = ("monitoring.alerts.additionalLabels.installed_version=fixed", "monitoring.alerts.additionalLabels.latest_version=fixed", "monitoring.alerts.additionalLabels.status=fixed")
check_rules(render((RULE,), overrides=short_version + version_reserved), [
    {"name": "versões distintas sobrevivem a additionalLabels durante rolling update",
     "input_series": [version_series("1+0x8"), version_series("1+0x8", installed_version="v0.8.0")],
     "promql_expr_test": [assertion("2m", outdated, [version_alert(), version_alert(installed_version="v0.8.0")])]},
])
check_rules(render((RULE,), overrides=("monitoring.cluster=",) + short_version + version_reserved + reserved_overrides), [
    {"name": "versão preserva clusters externos distintos e fallback local",
     "input_series": [version_series("1+0x8", cluster=cluster) for cluster in ["cluster-a", "cluster-b", None]],
     "promql_expr_test": [assertion("2m", outdated, [version_alert(cluster=cluster) for cluster in ["cluster-a", "cluster-b", "local"]])]},
])
print("Versão local info: cadência, recuperação/desconhecido, isolamento e labels reservados: OK")
