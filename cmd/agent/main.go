// nuvemcash-agent: coletor read-only de uso de Kubernetes para o nuvem.cash.
// Modo default = agente; subcomando "devsink" = receptor local de desenvolvimento (e2e).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		if err := http.ListenAndServe(":8081", devsink.Handler(os.Stdout)); err != nil {
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
	rc, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
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

	factory := informers.NewSharedInformerFactory(client, 10*time.Minute)
	podLister := factory.Core().V1().Pods().Lister()
	nodeLister := factory.Core().V1().Nodes().Lister()
	pvcLister := factory.Core().V1().PersistentVolumeClaims().Lister()
	pvLister := factory.Core().V1().PersistentVolumes().Lister()
	svcLister := factory.Core().V1().Services().Lister()
	rsLister := factory.Apps().V1().ReplicaSets().Lister()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	defer factory.Shutdown()
	slog.Info("agent started", "version", version, "clusterUid", clusterUID,
		"scrape", cfg.ScrapeInterval, "ship", cfg.ShipInterval)

	// Probe de vida (chart aponta liveness/readiness para cá).
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		_ = http.ListenAndServe(":8080", mux)
	}()

	shipper := ship.New(cfg.URL, cfg.Token, cfg.BufferWindows)
	window := aggregate.NewWindow(time.Now().UTC())

	scrape := time.NewTicker(cfg.ScrapeInterval)
	defer scrape.Stop()
	shipT := time.NewTicker(cfg.ShipInterval)
	defer shipT.Stop()

	for {
		select {
		case <-ctx.Done():
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
				for _, s := range samples {
					pod, err := podLister.Pods(s.Namespace).Get(s.PodName)
					if err != nil {
						continue // pod sumiu entre o scrape e o lookup
					}
					window.Observe(s, aggregate.ResolvePodMeta(pod, rsByKey))
				}
			}

		case now := <-shipT.C:
			end := now.UTC()
			usage := window.Close(end)
			nodes, _ := nodeLister.List(labels.Everything())
			pvcs, _ := pvcLister.List(labels.Everything())
			pvs, _ := pvLister.List(labels.Everything())
			pvByName := make(map[string]*corev1.PersistentVolume, len(pvs))
			for _, pv := range pvs {
				pvByName[pv.Name] = pv
			}
			svcs, _ := svcLister.List(labels.Everything())
			shipper.Enqueue(wire.Snapshot{
				SchemaVersion: wire.SchemaVersion,
				AgentVersion:  version,
				ClusterUID:    clusterUID,
				WindowStart:   window.Start(),
				WindowEnd:     end,
				Nodes:         collect.NodeInventory(nodes),
				Usage:         usage,
				PVCs:          collect.PVCInventory(pvcs, pvByName),
				LoadBalancers: collect.LBInventory(svcs),
			})
			window = aggregate.NewWindowFrom(window, end)
			if err := shipper.Flush(ctx); err != nil {
				slog.Warn("ship failed, windows kept in buffer",
					"pending", shipper.Pending(), "err", err)
			}
		}
	}
}
