#!/usr/bin/env bash
# e2e local da Fase 2: agente + devsink num cluster kind. Verifica que um snapshot com
# nodes e usage chega no devsink (contrato wire de ponta a ponta, sem backend).
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=agent-e2e
IMG=agent:e2e
NODE_IMAGE="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"

docker build -t "$IMG" .
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE"
kind load docker-image "$IMG" --name "$CLUSTER"
KCTX="kind-$CLUSTER"

# devsink no cluster (mesma imagem, subcomando).
kubectl --context "$KCTX" delete deploy devsink --ignore-not-found
kubectl --context "$KCTX" create deployment devsink --image="$IMG" -- /agent devsink
kubectl --context "$KCTX" patch deploy devsink --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
kubectl --context "$KCTX" expose deploy devsink --port 8081 --name devsink || true
kubectl --context "$KCTX" rollout status deploy/devsink --timeout=120s

# agente via chart, com janelas curtas para o teste (scrape 10s, envio 30s).
helm --kube-context "$KCTX" upgrade --install nuvemcash-agent charts/nuvemcash-agent \
  --namespace nuvemcash-system --create-namespace \
  --set image.repository=agent --set image.tag=e2e --set image.pullPolicy=Never \
  --set connection.token=e2e-token \
  --set connection.url=http://devsink.default.svc.cluster.local:8081 \
  --set scrapeInterval=10s --set shipInterval=30s
kubectl --context "$KCTX" -n nuvemcash-system rollout status deploy/nuvemcash-agent --timeout=120s

# aguarda até 3 janelas pelo snapshot com usage.
echo "aguardando snapshot no devsink..."
for i in $(seq 1 30); do
  LOGS=$(kubectl --context "$KCTX" logs deploy/devsink 2>/dev/null || true)
  if echo "$LOGS" | grep -q "snapshot cluster=" && echo "$LOGS" | grep -qE "usage=[1-9]"; then
    echo "== e2e OK =="
    echo "$LOGS" | tail -12
    exit 0
  fi
  sleep 10
done
echo "e2e FALHOU: nenhum snapshot com usage chegou" >&2
kubectl --context "$KCTX" -n nuvemcash-system logs deploy/nuvemcash-agent | tail -30 >&2
exit 1
