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

Sendo honesto sobre o RBAC: `nodes/proxy` é uma permissão ampla do kubelet (é o preço de
falar com ele via apiserver proxy em vez de TLS direto) — mas o agente só usa essa
permissão para um `GET stats/summary` por nó. Todo o resto do RBAC é `get`/`list`/`watch`
mínimo sobre os recursos listados acima.

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

## Monitoramento local

O agente oferece `/metrics` na porta 8080 por padrão, em formato Prometheus. As métricas
vêm do ciclo real de coleta e da fila existente, sem depender de a API aceitar o envio.
`/healthz` confirma que o processo está vivo; `/readyz` confirma somente a sincronização
inicial dos caches. Nenhuma das duas probes comprova saúde contínua de coleta ou envio.
Para desabilitar as métricas, use `monitoring.metrics.enabled=false` no chart ou
`NUVEMCASH_AGENT_METRICS_ENABLED=false` no processo.

O chart habilita `monitoring.podMonitor` e `monitoring.alerts` por padrão. Renderiza
`PodMonitor` e `PrometheusRule` **independentemente**, somente quando cada CRD está
presente e sua opção está habilitada. Sem CRDs, a instalação continua funcionando e as
NOTES apresentam a integração manual. O chart não instala operador nem stack de
monitoramento. As regras são do cluster do agente, com destinatário **local**; não são
alertas para o NOC central do nuvem.cash.

```yaml
monitoring:
  cluster: cluster-do-cliente
  environment: production
  podMonitor:
    labels:
      team: infraestrutura
    selector: {}                    # matchLabels adicionais dos pods
  alerts:
    labels:
      team: infraestrutura          # seleção do recurso pelo avaliador
    additionalLabels:
      owner: plataforma             # labels adicionais da notificação
    notificationMode: active        # controlled para testar o roteamento local
    failureSeconds: 600
    lossWindowSeconds: 600
    outdatedFor: 72h                 # aviso informativo de atualização
```

`namespace`, `release`, `environment` e `component=agent` são fixados no scrape e
filtrados exatamente nas regras. `monitoring.cluster`, quando preenchido, também é
fixado e filtrado. Quando vazio, preserva o cluster fornecido pelo coletor; as regras
agrupam os resultados por cluster sem misturar suas falhas. Séries sem esse label recebem
`cluster=local` apenas na avaliação local. **Antes de reunir séries de vários clusters,
configure identidades únicas em `monitoring.cluster` ou no coletor**: séries federadas
sem identidade já são indistinguíveis na origem. `additionalLabels` não substitui
identidade, condição, severidade, produto nem modo de notificação. `cluster`, `reason`,
`status`, `installed_version` e `latest_version` são reservados: os adicionais não
alteram dimensões da série/configuração. Para fixar o cluster da instalação, use
`monitoring.cluster`.

A instalação antiga com apenas o token continua válida mesmo em clusters com ambas as
CRDs. O label de release acrescentado ao pod isola o PodMonitor; o selector imutável do
Deployment foi preservado para permitir upgrade. Em upgrades de versões antigas, use
`--reset-then-reuse-values` para incorporar os defaults novos, em vez de `--reuse-values`.
Os nomes dos monitores e das regras incluem a release e pertencem ao namespace dela.

### Sinais e recuperação

Todas as métricas abaixo usam o prefixo `nuvemcash_agent_` e descrevem o processo atual.
Zero nos timestamps significa que o marco ainda não ocorreu desde a partida.

| Métricas | Significado |
| --- | --- |
| `collection_nodes_expected`, `collection_nodes_collected`, `collection_complete` | Nós esperados e nós cuja Summary API respondeu no último ciclo concluído. Uma rodada parcial não é completa; a primeira rodada ainda não concluída também não é completa. |
| `collection_failed`, `collection_failure_since_timestamp_seconds` | Há um nó esperado em falha; o timestamp é o início da falha corrente mais antiga. O relógio pertence ao nó: sucesso de outro nó ou envio parcial não o reinicia. Recupera quando o nó responde ou sai da lista esperada. |
| `collection_last_completed_timestamp_seconds`, `collection_last_success_timestamp_seconds`, `collection_duration_seconds` | Fim da última rodada, inclusive parcial; última rodada completa; duração observada. Uma rodada válida sem nós é completa e não implica envio impedido. |
| `ship_failed`, `ship_failure_since_timestamp_seconds`, `ship_last_success_timestamp_seconds` | Falha de transporte da janela retida; início dessa falha; última janela aceita por HTTP. Fila vazia não inventa sucesso nem falha. Aceitar uma janela não recupera outra ainda impedida. |
| `buffer_windows`, `buffer_bytes`, `buffer_oldest_age_seconds` | Quantidade de janelas agregadas e bytes comprimidos aguardando envio, incluindo o envio em andamento; idade desde o enfileiramento da mais antiga, não a idade dos dados no backend. |
| `dropped_windows_total{reason}`, `last_drop_timestamp_seconds{reason}` | Contagem e instante da última perda definitiva, com motivos limitados a `buffer_windows`, `buffer_bytes`, `encode` e `http_rejected`. Logs preservam `windowStart` e o erro/código HTTP quando aplicável. |
| `version_status{status,installed_version,latest_version}` | Uma série corrente com valor 1: `updated` (igual ou à frente), `outdated` (abaixo) ou `unknown` (comparação inválida/indisponível). Versões canônicas com `v`, sem build metadata; versão inválida é label vazio. |

A referência opcional vem de `latestAgentVersion` na resposta do envio existente, sem
polling do GitHub nem I/O adicional na API. Cada resposta HTTP aceita substitui a
referência anterior: corpo vazio/sem campo, JSON inválido, erro de leitura ou referência
inválida produzem `unknown`, inclusive após uma referência válida. Isso preserva o aceite
e a drenagem da janela, sem reenvio. A leitura é limitada a 4 KiB e cada versão a 128
bytes após acrescentar `v`; referências maiores são desconhecidas. Builds `dev` também
não permitem comparar. A comparação segue `golang.org/x/mod/semver`, como na API;
pré-release precede a release e build metadata não altera a precedência.

`NuvemcashAgentOutdated` tem severidade `info`, destinatário local e espera contínua
de `outdatedFor` (72h herdadas do aviso anterior, configuráveis; não é SLA). Só dispara
com comparação válida desatualizada. Atualização válida ou `unknown` encerram a condição
e reiniciam a espera; desconhecido não comprova atualização. A referência não tem prazo
de frescor: a API pode devolver seu último catálogo conhecido, e falhas de transporte
sem novo aceite não renovam a observação. O estado local se perde ao reiniciar o agente.

Um retry 429/5xx/rede mantém a janela no buffer e o início da falha. A regra só avisa
quando essa mesma falha alcança `failureSeconds` (dez minutos inicialmente), sem somar
outra espera de dez minutos. O aceite posterior remove a janela e recupera seu envio.
A rejeição definitiva descarta a janela e produz perda; não é retenção recuperável.
Perdas de buffer/encode e rejeições HTTP também integram a autotelemetria enviada à API.

A regra de perda usa o instante real do descarte e `lossWindowSeconds`, não o valor
histórico do counter. Assim, reset do counter ou reinício não cria uma nova ocorrência.
O encerramento desse aviso significa que a janela de observação terminou, **não que os
dados foram restaurados**. O buffer comprimido, os contadores e os marcos são apenas em
memória: reiniciar o processo perde as janelas restantes e o estado local. Logs e séries
já coletados podem preservar evidência externa, mas não recuperam a fila. Não há
persistência nova nem garantia de detecção de perda causada por término abrupto.

### Integração e verificação

Confirme as quatro etapas separadamente; criar os recursos não comprova que um alerta
chegará ao destinatário.

1. **Scrape:** acesse o endpoint e confirme o alvo saudável no coletor. A seleção dos
   pods usa namespace exato e labels da release, acrescidos de
   `monitoring.podMonitor.selector` quando configurado. Acrescente os labels usados
   nesse selector em `podLabels`; a identidade de nome/release é preservada.

   ```sh
   kubectl -n nuvemcash-system port-forward deployment/nuvemcash-agent 8080:8080
   curl -fsS http://localhost:8080/metrics
   ```

2. **Seleção:** confirme que o coletor seleciona o namespace e os labels do PodMonitor,
   e que o avaliador seleciona o namespace e os labels da PrometheusRule. São seletores
   distintos; ajuste `monitoring.podMonitor.labels` e `monitoring.alerts.labels` conforme
   a instalação existente. Os labels do recurso não são os labels dos alertas.
3. **Avaliação:** confirme as três regras carregadas e sem erro, com séries da identidade
   configurada. Sem CRD, obtenha as mesmas regras portáveis, usando a mesma versão do chart
   e os mesmos values da instalação. O exemplo usa `uv`/PyYAML apenas para extrair `.spec`:

   ```sh
   helm template nuvemcash-agent oci://ghcr.io/nuvemcash/charts/nuvemcash-agent \
     --namespace nuvemcash-system \
     --set connection.existingSecret=agent-token \
     --set monitoring.cluster=cluster-do-cliente \
     --set monitoring.environment=production \
     --api-versions monitoring.coreos.com/v1/PrometheusRule \
     --show-only templates/prometheusrule.yaml \
     | uv run --with PyYAML==6.0.3 python -c \
       'import sys,yaml; yaml.safe_dump(yaml.safe_load(sys.stdin)["spec"],sys.stdout,allow_unicode=True)' \
     > agent-rules.yaml
   promtool check rules agent-rules.yaml
   ```

   Configure esse arquivo no avaliador existente. `--api-versions` acima simula a CRD
   somente para exportação; não aplique uma PrometheusRule ao cluster sem a CRD. Para
   scrape manual, configure `/metrics` na porta 8080 e acrescente os labels
   `cluster=cluster-do-cliente`, `namespace=nuvemcash-system`,
   `release=nuvemcash-agent`, `environment=production` e `component=agent`.
4. **Notificação:** configure o destinatário local e valide disparo e recuperação em
   ambiente controlado. Os alertas incluem `product=nuvemcash`, `component=agent`,
   `condition`, `severity=warning`, identidade da instalação e `notification_mode`.
   O modo `controlled` é um label para a rota local de teste, não uma implementação de
   entrega. A janela de perda e a recuperação de coleta/envio têm significados diferentes.

### Checks do monitoramento

O runner usa Helm, `promtool` **3.15.0** e schemas oficiais do Prometheus Operator
v0.85.0; `uv` resolve as duas dependências de teste fixadas no próprio script.

```sh
helm lint charts/nuvemcash-agent --set connection.existingSecret=agent-token
PROMTOOL=promtool uv run charts/nuvemcash-agent/tests/test_alerts.py
PROMTOOL=promtool go test -race ./cmd/agent ./internal/ship ./internal/collect ./internal/aggregate ./internal/config
```

As fixtures cobrem CRDs independentes, token-only, seleção, isolamento entre releases,
namespaces e clusters, limites personalizados, disparo/recuperação e resets. O teste HTTP
executa `promtool check metrics` contra a resposta do endpoint real e verifica acesso
concorrente durante envio. A validação final da entrega e do destinatário depende do
monitoramento instalado no cluster.
