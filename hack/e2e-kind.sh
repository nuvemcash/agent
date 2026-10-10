#!/usr/bin/env bash
# e2e local num cluster kind, sem backend: o devsink faz o papel da api do nuvem.cash.
#
# Fase 2: um snapshot com nodes, usage e a resolução Pod→RS→Deployment chega ao devsink.
# Atualização automática (ADR 0017, api#326): o updater, a partir de um registry OCI
# local, aplica o chart inteiro N→N+1 preservando token e values, reverte uma imagem
# quebrada (rolled_back), é recusado ao pedir RBAC acima do teto (rejected_by_ceiling, com
# a ClusterRole intacta), atualiza a própria imagem e se abstém quando a release é do Flux
# (abstained, sem tocar na release). As imagens do cenário são do build e2e (tag e2e), cujo
# updater pula a Verificação de origem (charts locais não têm assinatura); um updater do build
# normal recusa o chart sem assinatura (signature_invalid, release intocada). Uma release cujo último apply foi do Helm 4 CLI (SSA,
# field manager "helm") é atualizada sem conflito de ownership (api#316). Desligada, o coletor relata abstained/disabled. A
# troca de pods não mede os mesmos segundos duas vezes.
#
# CLUSTER e REGISTRY permitem isolar execuções paralelas; o script não apaga nada ao fim.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-agent-e2e}
REGISTRY=${REGISTRY:-agent-e2e-registry}
NODE_IMAGE="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
REPO=ghcr.io/nuvemcash/agent # o repositório padrão: registry espelhado é abstenção
NS=nuvemcash-system
REL=nuvemcash-agent
SCRAPE=10

# Versões do cenário: N, N+1, N+1 com imagem inexistente, N+1 pedindo secrets (fora do teto) e
# N+2 aplicada sobre a release em SSA do Helm 4 CLI e N+3 sem assinatura, para o build normal.
V0=0.1.0 V1=0.1.1 VBROKEN=0.1.2 VCEILING=0.1.3 VSSA=0.1.4 VSIG=0.1.5

fail() {
  echo "e2e FALHOU: $*" >&2
  kubectl -n "$NS" get pods,jobs >&2 || true
  kubectl logs deploy/devsink 2>/dev/null | tail -20 >&2 || true
  exit 1
}

# Imagens: N e N+1 são builds distintos (a versão viaja nos snapshots e separa os pods na
# checagem de sobreposição); a do teto e a do SSA reaproveitam N+1; a quebrada nunca é carregada.
docker build -q --build-arg VERSION=$V0 --build-arg BUILD_TAGS=e2e -t $REPO:$V0 . >/dev/null
docker build -q --build-arg VERSION=$V1 --build-arg BUILD_TAGS=e2e -t $REPO:$V1 . >/dev/null
# Build normal (o do release): só roda o updater do cenário de assinatura.
docker build -q --build-arg VERSION=$VSSA -t $REPO:release-build . >/dev/null
docker tag $REPO:$V1 $REPO:$VCEILING
docker tag $REPO:$V1 $REPO:$VSSA
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE"
kind load docker-image $REPO:$V0 $REPO:$V1 $REPO:$VCEILING $REPO:$VSSA $REPO:release-build --name "$CLUSTER"

# Registra o contexto do kind explicitamente, num KUBECONFIG PRÓPRIO. Não é higiene: numa
# máquina de trabalho o kubeconfig ambiente costuma apontar para um cluster de PRODUÇÃO, e
# um e2e que dependa do contexto default é um acidente esperando acontecer.
export KUBECONFIG="${TMPDIR:-/tmp}/kubeconfig-$CLUSTER"
kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
KCTX="kind-$CLUSTER"
kubectl() { command kubectl --context "$KCTX" "$@"; }
helm() { command helm --kube-context "$KCTX" "$@"; }

# Registry OCI local na rede do kind; os pods o alcançam pelo IP do container.
docker inspect "$REGISTRY" >/dev/null 2>&1 ||
  docker run -d --name "$REGISTRY" --network kind -p 127.0.0.1::5000 registry:2 >/dev/null
REG_PORT=$(docker port "$REGISTRY" 5000/tcp | head -1 | cut -d: -f2)
REG_IP=$(docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$REGISTRY")
CHART_REF="oci://$REG_IP:5000/charts/$REL"

# Empacota e publica cada versão; o digest do manifesto é o que a "api" oferece.
WORK=$(mktemp -d)
for v in $V0 $V1 $VBROKEN $VCEILING $VSSA $VSIG; do mkdir -p "$WORK/$v" && cp -R charts/$REL "$WORK/$v/"; done
# A versão do teto pede list em secrets na ClusterRole do coletor: além do teto do updater.
python3 - "$WORK/$VCEILING/$REL/templates/rbac.yaml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
extra = '  - apiGroups: [""]\n    resources: ["secrets"]\n    verbs: ["list"]\n---'
assert '    verbs: ["get"]\n---' in s
open(p, "w").write(s.replace('    verbs: ["get"]\n---', '    verbs: ["get"]\n' + extra, 1))
PY
for v in $V0 $V1 $VBROKEN $VCEILING $VSSA $VSIG; do
  command helm package "$WORK/$v/$REL" --version "$v" --app-version "$v" -d "$WORK/$v" >/dev/null
  command helm push "$WORK/$v/$REL-$v.tgz" "oci://127.0.0.1:$REG_PORT/charts" --plain-http 2>&1 |
    awk '/^Digest:/ {print $2}' >"$WORK/$v/digest"
  [ -s "$WORK/$v/digest" ] || fail "push do chart $v sem digest"
done

# devsink no cluster (mesma imagem, subcomando), com o catálogo de releases em ordem: a
# cada execução do updater ele oferece a primeira acima da instalada que não falhou — como
# a api, que não oferece de novo uma versão que falhou no cluster. Assim os cenários abaixo
# andam N→N+1, depois a quebrada, depois a do teto, sem reiniciar o devsink (o que perderia
# os logs que as asserções leem).
RELEASES=$(for v in $V1 $VBROKEN $VCEILING $VSSA $VSIG; do printf '%s@%s,' $v "$(cat "$WORK/$v/digest")"; done)
kubectl delete deploy devsink --ignore-not-found
kubectl create deployment devsink --image=$REPO:$V0 -- /agent devsink
kubectl set env deploy/devsink "NUVEMCASH_DEVSINK_AGENT_RELEASES=$RELEASES" >/dev/null
kubectl patch deploy devsink --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
kubectl expose deploy devsink --port 8081 --name devsink || true
kubectl rollout status deploy/devsink --timeout=120s

# Agente N via chart, instalado como pelo Helm 3 (client-side apply), com janelas curtas
# (scrape 10s, envio 30s). Probes rápidas mantêm scrapeInterval > startup + readiness, a
# condição para a troca de pods não contar em dobro (templates/deployment.yaml).
helm uninstall $REL -n $NS --wait >/dev/null 2>&1 || true
kubectl -n $NS delete job --all --ignore-not-found >/dev/null 2>&1 || true
helm install $REL "$WORK/$V0/$REL-$V0.tgz" --namespace $NS --create-namespace --server-side=false \
  --set image.pullPolicy=Never \
  --set connection.token=e2e-token \
  --set connection.url=http://devsink.default.svc.cluster.local:8081 \
  --set scrapeInterval=${SCRAPE}s --set shipInterval=30s \
  --set probes.startup.periodSeconds=1 --set probes.readiness.periodSeconds=2 \
  --set autoUpgrade.chart="$CHART_REF" --set autoUpgrade.plainHTTP=true --set autoUpgrade.timeout=60s
kubectl -n $NS rollout status deploy/$REL --timeout=120s
# O agendamento de hora em hora não pode disparar no meio do teste: as execuções abaixo
# são manuais, a partir do mesmo CronJob.
kubectl -n $NS patch cronjob $REL-updater -p '{"spec":{"suspend":true}}' >/dev/null

# Fase 2: aguarda até 3 janelas pelo snapshot com usage E com a resolução de workload.
#
# A asserção de workload NÃO é decoração: o transform do informer poda os Pods antes de eles
# entrarem no cache, e podar demais quebra a resolução Pod→RS→Deployment em SILÊNCIO — o uso
# passaria a ser atribuído ao Pod cru em vez do Deployment, sem erro nenhum, mudando a conta
# do cliente. O devsink já é um Deployment, então ele mesmo é a cobaia.
wait_snapshot() { # versão do agente
  echo "aguardando snapshot do agente $1 no devsink..."
  for _ in $(seq 1 30); do
    LOGS=$(kubectl logs deploy/devsink 2>/dev/null || true)
    if echo "$LOGS" | grep -E "snapshot cluster=.* agent=$1 .*usage=[1-9]" >/dev/null; then
      echo "$LOGS" | grep -E "default/devsink \(Deployment\)" >/dev/null ||
        fail "snapshot chegou, mas o workload não foi resolvido como Deployment (poda do informer? ver trimCached)"
      return 0
    fi
    sleep 10
  done
  kubectl -n $NS logs deploy/$REL | tail -30 >&2
  fail "nenhum snapshot com usage do agente $1 chegou"
}
wait_snapshot $V0
echo "== Fase 2 OK (snapshot + resolução Pod→RS→Deployment) =="

run_updater() { # nome do job
  kubectl -n $NS create job "$1" --from=cronjob/$REL-updater >/dev/null
  local st=""
  for _ in $(seq 1 120); do # até 10min; o job tem backoffLimit 0, falha é definitiva
    st=$(kubectl -n $NS get job "$1" -o jsonpath='{.status.succeeded}/{.status.failed}')
    [ "$st" = "1/" ] || [ "${st#*/}" = 1 ] && break
    sleep 5
  done
  if [ "$st" != "1/" ]; then
    kubectl -n $NS logs "job/$1" | tail -40 >&2
    fail "job $1 do updater não completou ($st)"
  fi
  kubectl -n $NS logs "job/$1" | grep -E "agent update|level=(WARN|ERROR)" || true
}
want_outcome() { # versão desfecho
  kubectl logs deploy/devsink | grep "agent-update outcome version=$1 outcome=$2" >/dev/null ||
    fail "devsink não recebeu o desfecho $2 da versão $1"
}
release() { helm get metadata $REL -n $NS -o json; }
image_of() { kubectl -n $NS get "$1" -o jsonpath="$2"; }
DEP_IMG='{.spec.template.spec.containers[0].image}'
CJ_IMG='{.spec.jobTemplate.spec.template.spec.containers[0].image}'

# --- N→N+1: chart inteiro, token e values preservados, a própria imagem atualizada.
run_updater upd-happy
want_outcome $V1 applied
META=$(release)
[ "$(jq -r .version <<<"$META")" = $V1 ] && [ "$(jq -r .status <<<"$META")" = deployed ] ||
  fail "release não está em $V1 deployed: $META"
[ "$(jq -r .applyMethod <<<"$META")" = csa ] || fail "release mudou de client-side apply: $META"
[ "$(kubectl -n $NS get secret $REL-token -o jsonpath='{.data.token}' | base64 -d)" = e2e-token ] ||
  fail "token não foi preservado"
[ "$(helm get values $REL -n $NS -o json | jq -r .scrapeInterval)" = ${SCRAPE}s ] || fail "values do usuário perdidos"
[ "$(image_of deploy/$REL "$DEP_IMG")" = $REPO:$V1 ] || fail "Deployment não está na imagem $V1"
[ "$(image_of cronjob/$REL-updater "$CJ_IMG")" = $REPO:$V1 ] || fail "o updater não atualizou a própria imagem"
[ "$(kubectl -n $NS get deploy $REL -o jsonpath='{.spec.strategy.rollingUpdate.maxUnavailable}')" = 0 ] ||
  fail "Deployment sem maxUnavailable 0"
kubectl -n $NS rollout status deploy/$REL --timeout=120s

# Sobreposição: os segundos medidos pelo pod antigo terminam no fim da última janela dele;
# os do novo começam um scrapeInterval depois do início da primeira janela dele. Se o
# primeiro passasse do segundo, o ingest (que soma as janelas por hora) contaria em dobro.
wait_snapshot $V1
LOGS=$(kubectl logs deploy/devsink)
OLD_END=$(grep -o "agent=$V0 .*" <<<"$LOGS" | sed -E 's/.* end=([0-9]+).*/\1/' | sort -n | tail -1 || true)
NEW_START=$(grep -o "agent=$V1 .*" <<<"$LOGS" | sed -E 's/.* start=([0-9]+).*/\1/' | sort -n | head -1 || true)
[ -n "$OLD_END" ] && [ -n "$NEW_START" ] || fail "janelas dos dois pods não chegaram"
echo "sobreposição: antigo mede até $OLD_END, novo a partir de $((NEW_START + SCRAPE))"
[ "$OLD_END" -le $((NEW_START + SCRAPE)) ] || fail "pods mediram os mesmos segundos (contaria em dobro)"
echo "== atualização N→N+1 OK =="

# --- Imagem quebrada: o Helm reverte e o desfecho chega como rolled_back.
run_updater upd-broken
want_outcome $VBROKEN rolled_back
[ "$(release | jq -r .version)" = $V1 ] || fail "release não voltou a $V1"
[ "$(image_of deploy/$REL "$DEP_IMG")" = $REPO:$V1 ] || fail "Deployment não voltou à imagem $V1"
echo "== imagem quebrada revertida OK =="

# --- RBAC fora do teto: o apiserver recusa, a ClusterRole fica intacta.
RULES_BEFORE=$(kubectl get clusterrole $REL -o jsonpath='{.rules}')
run_updater upd-ceiling
want_outcome $VCEILING rejected_by_ceiling
[ "$(kubectl get clusterrole $REL -o jsonpath='{.rules}')" = "$RULES_BEFORE" ] || fail "ClusterRole do coletor mudou"
[ "$(release | jq -r .version)" = $V1 ] || fail "release não ficou em $V1"
echo "== RBAC acima do teto recusado OK =="

# --- Release do Flux: o marcador de posse nos recursos leva à abstenção, sem escrita.
REV_BEFORE=$(helm history $REL -n $NS -o json | jq length)
kubectl -n $NS label deploy/$REL helm.toolkit.fluxcd.io/name=agent >/dev/null
run_updater upd-flux
want_outcome $V1 abstained
kubectl logs deploy/devsink | grep "agent-update outcome version=$V1 outcome=abstained reason=\"gitops_flux\"" >/dev/null ||
  fail "devsink não recebeu o motivo gitops_flux"
[ "$(helm history $REL -n $NS -o json | jq length)" = "$REV_BEFORE" ] || fail "a abstenção escreveu na release"
[ "$(release | jq -r .version)" = $V1 ] || fail "release não ficou em $V1"
echo "== abstenção sob Flux OK =="

# --- Release do Helm 4 CLI em server-side apply: o último apply é do field manager "helm".
# Um updater com manager próprio conflita na imagem e no token (api#316), e o rollback
# conflita igual, deixando a release em failed.
kubectl -n $NS label deploy/$REL helm.toolkit.fluxcd.io/name- >/dev/null
helm upgrade $REL "$WORK/$V1/$REL-$V1.tgz" -n $NS --reuse-values --server-side=true >/dev/null
[ "$(release | jq -r .applyMethod)" = ssa ] || fail "release não passou a server-side apply: $(release)"
run_updater upd-ssa
want_outcome $VSSA applied
META=$(release)
[ "$(jq -r .version <<<"$META")" = $VSSA ] && [ "$(jq -r .status <<<"$META")" = deployed ] ||
  fail "release não está em $VSSA deployed: $META"
[ "$(image_of deploy/$REL "$DEP_IMG")" = $REPO:$VSSA ] || fail "Deployment não está na imagem $VSSA"
[ "$(image_of cronjob/$REL-updater "$CJ_IMG")" = $REPO:$VSSA ] || fail "o updater não atualizou a própria imagem"
[ "$(kubectl -n $NS get secret $REL-token -o jsonpath='{.data.token}' | base64 -d)" = e2e-token ] ||
  fail "token não foi preservado"
kubectl -n $NS rollout status deploy/$REL --timeout=120s
echo "== release em SSA do Helm 4 CLI atualizada OK =="

# --- Verificação de origem: o updater do build normal recusa N+3, que não tem assinatura.
REV=$(release | jq -r .revision)
kubectl -n $NS create job upd-signature --from=cronjob/$REL-updater --dry-run=client -o json |
  jq --arg img $REPO:release-build '.spec.template.spec.containers[0].image = $img |
    .spec.template.spec.containers[0].imagePullPolicy = "Never"' |
  kubectl apply -f - >/dev/null
kubectl -n $NS wait --for=condition=complete job/upd-signature --timeout=180s ||
  fail "job do updater do build normal não completou"
want_outcome $VSIG "signature_invalid"
META=$(release)
[ "$(jq -r .version <<<"$META")" = $VSSA ] && [ "$(jq -r .revision <<<"$META")" = "$REV" ] ||
  fail "chart sem assinatura mexeu na release: $META"
echo "== chart sem assinatura recusado pela Verificação de origem OK =="

# --- Desligada: sem CronJob não há updater para relatar; o coletor relata ao subir.
helm upgrade $REL "$WORK/$V1/$REL-$V1.tgz" -n $NS --reuse-values --set autoUpgrade.enabled=false >/dev/null
kubectl -n $NS rollout status deploy/$REL --timeout=120s
kubectl -n $NS get cronjob $REL-updater >/dev/null 2>&1 && fail "autoUpgrade.enabled=false ainda tem CronJob"
for _ in $(seq 1 30); do
  kubectl logs deploy/devsink | grep "agent-update outcome version=$V1 outcome=abstained reason=\"disabled\"" >/dev/null && break
  sleep 2
done
want_outcome $V1 "abstained reason=\"disabled\""
echo "== desligada relatada pelo coletor OK =="

echo "== e2e OK =="
