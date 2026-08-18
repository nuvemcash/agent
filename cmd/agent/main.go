// nuvemcash-agent: coletor read-only de uso de Kubernetes para o nuvem.cash.
// Modo default = agente; subcomando "devsink" = receptor local de desenvolvimento (e2e).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nuvemcash/agent/internal/aggregate"
	"github.com/nuvemcash/agent/internal/collect"
	"github.com/nuvemcash/agent/internal/config"
	"github.com/nuvemcash/agent/internal/devsink"
	"github.com/nuvemcash/agent/internal/ship"
	"github.com/nuvemcash/agent/wire"
)

var version = "dev" // injetada por -ldflags "-X main.version=vX.Y.Z"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "devsink" {
		slog.Info("devsink listening", "addr", ":8081", "path", wire.Path)
		srv := &http.Server{Addr: ":8081", Handler: devsink.Handler(os.Stdout), ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("devsink", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("agent exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// As probes sobem ANTES de qualquer trabalho de partida. Sincronizar os informers
	// leva dezenas de segundos em cluster grande, e enquanto a porta não estiver no ar o
	// kubelet mata o pod por liveness sem nunca deixá-lo terminar de subir — foi o que
	// pôs o agente em CrashLoopBackOff num cluster de 19 nós / 512 pods / 2131 RS.
	var ready atomic.Bool
	go serveProbes(&ready)

	rc, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	// O default do client-go (5 QPS / 10 burst) é pequeno demais aqui: cada ciclo dispara
	// um nodes/proxy por nó, em rajada, além dos watches dos informers. Com o default
	// já se observou throttling de 8s numa única chamada de scrape.
	rc.QPS, rc.Burst = 50, 100
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}

	// Fingerprint do cluster: UID do namespace kube-system (convenção k8s.cluster.uid).
	ks, err := client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return err
	}
	clusterUID := string(ks.UID)

	factory := informers.NewSharedInformerFactoryWithOptions(client, 10*time.Minute,
		informers.WithTransform(trimCached))
	podLister := factory.Core().V1().Pods().Lister()
	nodeLister := factory.Core().V1().Nodes().Lister()
	pvcLister := factory.Core().V1().PersistentVolumeClaims().Lister()
	pvLister := factory.Core().V1().PersistentVolumes().Lister()
	svcLister := factory.Core().V1().Services().Lister()
	rsLister := factory.Apps().V1().ReplicaSets().Lister()
	factory.Start(ctx.Done())
	defer factory.Shutdown()
	// Cache não sincronizado é erro, não um caso a seguir em frente: os listers
	// devolveriam listas vazias e o snapshot sairia com inventário zerado.
	for typ, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			return fmt.Errorf("cache do informer %v não sincronizou", typ)
		}
	}
	ready.Store(true)
	slog.Info("agent started", "version", version, "clusterUid", clusterUID,
		"scrape", cfg.ScrapeInterval, "ship", cfg.ShipInterval)

	shipper := ship.New(cfg.URL, cfg.Token, cfg.BufferWindows, cfg.BufferBytes)
	window := aggregate.NewWindow(time.Now().UTC())
	// Duração do último ciclo de scrape, para viajar na autotelemetria. Só o loop principal
	// escreve e lê, na mesma goroutine.
	var lastScrape time.Duration

	// buildSnapshot monta o envelope da janela corrente e a rola para a próxima.
	buildSnapshot := func(end time.Time) wire.Snapshot {
		usage := window.Close(end)
		sampledSeconds := window.NodeSampledSeconds() // captura ANTES de rolar a janela abaixo
		nodes, _ := nodeLister.List(labels.Everything())
		pvcs, _ := pvcLister.List(labels.Everything())
		pvs, _ := pvLister.List(labels.Everything())
		pvByName := make(map[string]*corev1.PersistentVolume, len(pvs))
		for _, pv := range pvs {
			pvByName[pv.Name] = pv
		}
		svcs, _ := svcLister.List(labels.Everything())
		nodeInventory := collect.NodeInventory(nodes)
		for i := range nodeInventory {
			nodeInventory[i].SampledSeconds = sampledSeconds[nodeInventory[i].Name]
		}
		snap := wire.Snapshot{
			SchemaVersion: wire.SchemaVersion,
			AgentVersion:  version,
			ClusterUID:    clusterUID,
			WindowStart:   window.Start(),
			WindowEnd:     end,
			Nodes:         nodeInventory,
			Usage:         usage,
			PVCs:          collect.PVCInventory(pvcs, pvByName),
			LoadBalancers: collect.LBInventory(svcs),
			// Ocupação lida ANTES de esta janela entrar na fila — é a fila que ela
			// encontrou, que é o que interessa para ver a fila crescer.
			Agent: &wire.AgentHealth{
				DroppedWindows:    shipper.Dropped(),
				BufferedWindows:   shipper.Pending(),
				BufferedBytes:     shipper.PendingBytes(),
				ScrapeRoundMillis: lastScrape.Milliseconds(),
			},
		}
		window = aggregate.NewWindowFrom(window, end)
		return snap
	}

	scrape := time.NewTicker(cfg.ScrapeInterval)
	defer scrape.Stop()
	shipT := time.NewTicker(cfg.ShipInterval)
	defer shipT.Stop()

	for {
		select {
		case <-ctx.Done():
			// Shutdown gracioso: entrega a janela corrente antes de sair — sem isso,
			// todo rolling update descartaria até um ShipInterval de uso observado.
			shipper.Enqueue(buildSnapshot(time.Now().UTC()))
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := shipper.Flush(flushCtx); err != nil {
				slog.Warn("final flush failed", "pending", shipper.Pending(), "err", err)
			}
			return nil

		case <-scrape.C:
			// Índice RS para resolução Pod→Deployment nesta rodada.
			rss, _ := rsLister.List(labels.Everything())
			rsByKey := make(map[string]*appsv1.ReplicaSet, len(rss))
			for _, rs := range rss {
				rsByKey[rs.Namespace+"/"+rs.Name] = rs
			}
			nodes, _ := nodeLister.List(labels.Everything())
			roundStart := time.Now()
			for _, r := range scrapeNodes(ctx, client, nodes) {
				// Cobertura do nó = intervalo entre scrapes bem-sucedidos DELE, contado
				// uma vez aqui. Contar dentro do laço de pods multiplicaria a cobertura
				// pela quantidade de pods do nó.
				//
				// As amostras são aplicadas SERIALMENTE, aqui, mesmo tendo sido coletadas
				// em paralelo: Window não é thread-safe, e uma corrida em sampled_s
				// corromperia o rateio (o denominador do custo por nó).
				window.ObserveNode(r.node, r.at)
				for _, s := range r.samples {
					pod, err := podLister.Pods(s.Namespace).Get(s.PodName)
					if err != nil {
						continue // pod sumiu entre o scrape e o lookup
					}
					window.Observe(s, aggregate.ResolvePodMeta(pod, rsByKey))
				}
			}
			lastScrape = time.Since(roundStart)

		case now := <-shipT.C:
			shipper.Enqueue(buildSnapshot(now.UTC()))
			if err := shipper.Flush(ctx); err != nil {
				slog.Warn("ship failed, windows kept in buffer",
					"pending", shipper.Pending(), "err", err)
			}
		}
	}
}

// serveProbes expõe /healthz (o processo está vivo) e /readyz (os caches sincronizaram).
// São endpoints distintos de propósito: durante a partida o agente está vivo mas ainda não
// pronto, e responder 200 no liveness desde o primeiro instante é o que impede o kubelet de
// reiniciá-lo em loop antes de ele chegar ao fim da sincronização.
func serveProbes(ready *atomic.Bool) {
	srv := &http.Server{Addr: ":8080", Handler: probeMux(ready), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("probe listener failed", "err", err)
	}
}

func probeMux(ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "sincronizando caches", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// trimCached poda os objetos ANTES de eles entrarem no cache do informer. O cache é o maior
// consumidor de memória do agente num cluster grande, e tudo o que ele guarda além do que a
// agregação lê é peso morto.
//
// ATENÇÃO ao mexer aqui: podar demais quebra a resolução Pod→RS→Deployment em SILÊNCIO — o
// workload passa a ser atribuído errado, sem erro nenhum. O hack/e2e-kind.sh cobre essa
// resolução de ponta a ponta e é a única rede contra isso.
func trimCached(obj any) (any, error) {
	// managedFields é metadado de server-side apply que ninguém aqui lê e costuma ser dos
	// maiores campos de qualquer objeto. Vale para TODOS os tipos.
	if m, ok := obj.(metav1.Object); ok {
		m.SetManagedFields(nil)
	}
	switch o := obj.(type) {
	case *appsv1.ReplicaSet:
		// Do RS só interessam as ownerReferences (Pod→RS→Deployment), e o template responde
		// por quase todo o peso: num cluster com 2131 RS a LIST inicial passa de 15 MB.
		o.Spec.Template = corev1.PodTemplateSpec{}
		o.Annotations = nil
		return o, nil
	case *corev1.Pod:
		// O ÚNICO consumidor de *corev1.Pod é aggregate.ResolvePodMeta, que lê: Namespace,
		// Name, Labels, OwnerReferences, Spec.NodeName e os Resources.Requests dos
		// containers. Tudo o mais é descartável — e num cluster com 3.000–8.000 pods de
		// 15–30 KB cada, "tudo o mais" é a maior parte da memória do agente.
		requests := make([]corev1.Container, 0, len(o.Spec.Containers))
		for _, c := range o.Spec.Containers {
			requests = append(requests, corev1.Container{
				Resources: corev1.ResourceRequirements{Requests: c.Resources.Requests},
			})
		}
		o.Spec = corev1.PodSpec{NodeName: o.Spec.NodeName, Containers: requests}
		o.Status = corev1.PodStatus{}
		o.Annotations = nil
		return o, nil
	}
	return obj, nil
}

// scrapeConcurrency limita os scrapes simultâneos de kubelet. O laço serial anterior
// gastava até 30s por nó no pior caso: a 500 ms/nó, 132 nós levavam ~66s e passavam do
// ScrapeInterval de 60s, fazendo o ticker DESCARTAR ticks — e cobertura perdida empurra
// custo para "não monitorado", ou seja, mexe na conta do cliente. O limite fica dentro do
// QPS/Burst já elevado do client (50/100), para o paralelismo não virar throttling.
const scrapeConcurrency = 12

// nodeSample é o resultado do scrape de UM nó, com o instante em que ele foi observado.
type nodeSample struct {
	node    string
	at      time.Time
	samples []collect.PodSample
}

// scrapeNodes varre os nós em paralelo e devolve só os que responderam. O instante de
// observação é capturado DENTRO da goroutine, no fim do scrape daquele nó: usar um relógio
// único depois do Wait daria a todos os nós a hora do mais lento, deslocando a cobertura.
//
// Erro de um nó não aborta os demais — um kubelet fora do ar não pode zerar o ciclo.
func scrapeNodes(ctx context.Context, client kubernetes.Interface, nodes []*corev1.Node) []nodeSample {
	var (
		mu   sync.Mutex
		out  = make([]nodeSample, 0, len(nodes))
		wg   sync.WaitGroup
		slot = make(chan struct{}, scrapeConcurrency)
	)
	for _, n := range nodes {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			slot <- struct{}{}
			defer func() { <-slot }()
			samples, err := collect.ScrapeNode(ctx, client, name)
			if err != nil {
				slog.Warn("node scrape failed", "node", name, "err", err)
				return
			}
			mu.Lock()
			out = append(out, nodeSample{node: name, at: time.Now().UTC(), samples: samples})
			mu.Unlock()
		}(n.Name)
	}
	wg.Wait()
	return out
}
