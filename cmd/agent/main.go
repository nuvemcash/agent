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
		informers.WithTransform(trimReplicaSetTemplate))
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

	shipper := ship.New(cfg.URL, cfg.Token, cfg.BufferWindows)
	window := aggregate.NewWindow(time.Now().UTC())

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
			for _, n := range nodes {
				samples, err := collect.ScrapeNode(ctx, client, n.Name)
				if err != nil {
					slog.Warn("node scrape failed", "node", n.Name, "err", err)
					continue
				}
				// Cobertura do nó = intervalo entre scrapes bem-sucedidos DELE, contado
				// uma vez aqui. Contar dentro do laço de pods multiplicaria a cobertura
				// pela quantidade de pods do nó.
				window.ObserveNode(n.Name, time.Now().UTC())
				for _, s := range samples {
					pod, err := podLister.Pods(s.Namespace).Get(s.PodName)
					if err != nil {
						continue // pod sumiu entre o scrape e o lookup
					}
					window.Observe(s, aggregate.ResolvePodMeta(pod, rsByKey))
				}
			}

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

// trimReplicaSetTemplate descarta o PodTemplateSpec dos ReplicaSets antes de eles entrarem
// no cache do informer. Do RS só interessam as ownerReferences (Pod→RS→Deployment), e o
// template responde por quase todo o peso do objeto: num cluster com 2131 RS a LIST inicial
// passa de 15 MB. Os demais tipos passam intactos — a factory aplica o transform a todos.
func trimReplicaSetTemplate(obj any) (any, error) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		return obj, nil
	}
	rs.Spec.Template = corev1.PodTemplateSpec{}
	return rs, nil
}
