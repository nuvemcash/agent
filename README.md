# nuvemcash-agent

> Read-only Kubernetes usage collector for [nuvem.cash](https://nuvem.cash) — ships
> aggregated CPU/memory usage, node & PVC inventory to your nuvem.cash workspace. No
> secrets are read; nothing is mutated in your cluster. Apache-2.0.

Agente de coleta do nuvem.cash para clusters Kubernetes. Ele observa a kube API
(somente leitura) e o kubelet (via apiserver proxy), agrega o uso por workload em
janelas de 5 minutos e envia para o nuvem.cash, onde o custo real da fatura do provedor
é rateado por namespace e workload.

## Instalação

O comando é exibido na tela de conexão do cluster no nuvem.cash (Contas → sua conta →
Clusters), já com o token embutido:

```bash
helm upgrade --install nuvemcash-agent oci://ghcr.io/nuvemcash/charts/nuvemcash-agent \
  --namespace nuvemcash-system --create-namespace \
  --set connection.token=<TOKEN>
```

Se preferir não passar o token via `--set` (shell history), crie um Secret e use
`--set connection.existingSecret=<nome>` (chave `token`).

Token rotacionado: o mesmo comando de instalação acima (`helm upgrade` com o token novo)
já aplica o Secret e reinicia o agente automaticamente. Com `existingSecret`, o rollout do
Deployment fica por conta de quem opera o Secret externo (`kubectl rollout restart
deployment/nuvemcash-agent -n nuvemcash-system` após atualizá-lo).

## O que o agente coleta

- Inventário de nós (capacidade, allocatable, labels, providerID), PVCs e Services LB
- Uso de CPU/memória por pod (kubelet Summary API), agregado por workload
- Nada de Secrets/ConfigMaps; RBAC estritamente somente-leitura

## Requisitos

Kubernetes ≥ 1.28 · Helm ≥ 3.8 · saída HTTPS para o endpoint do nuvem.cash.

## Desenvolvimento

Teste e2e local — builda a imagem, sobe um cluster [kind](https://kind.sigs.k8s.io/),
instala o devsink (receptor de desenvolvimento embutido no próprio binário, `agent
devsink`) e o chart apontando pra ele, e aguarda até um snapshot com uso chegar:

```bash
./hack/e2e-kind.sh
```

Critério de aceite da Fase 2. O script não apaga o cluster ao final; para limpar:
`kind delete cluster --name agent-e2e`.
