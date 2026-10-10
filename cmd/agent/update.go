package main

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/cli"
	"k8s.io/client-go/kubernetes"

	"github.com/nuvemcash/agent/internal/config"
	"github.com/nuvemcash/agent/internal/update"
)

// runDeadline é o limite de uma execução do updater e tem de casar com o
// activeDeadlineSeconds do CronJob (templates/updater.yaml). Release em pending-* há mais
// tempo que isso ficou órfã: o Job morreu no meio do upgrade.
const runDeadline = 15 * time.Minute

func runUpdate() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadUpdater()
	if err != nil {
		return err
	}
	settings := cli.New()
	settings.SetNamespace(cfg.Namespace)
	helm := action.NewConfiguration()
	if err := helm.Init(settings.RESTClientGetter(), cfg.Namespace, "secret"); err != nil {
		return err
	}
	rc, err := settings.RESTClientGetter().ToRESTConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	u := update.Updater{
		API:          update.API{URL: cfg.URL, Token: cfg.Token, Client: &http.Client{Timeout: 30 * time.Second}},
		Helm:         helm,
		Kube:         kube,
		Release:      cfg.Release,
		Namespace:    cfg.Namespace,
		Fetch:        update.OCIFetcher{Chart: cfg.Chart, PlainHTTP: cfg.PlainHTTP}.Fetch,
		Verify:       update.OriginVerifier{Chart: cfg.Chart, PlainHTTP: cfg.PlainHTTP}.Verify,
		Timeout:      cfg.Timeout,
		PendingLimit: runDeadline,
	}
	return u.Run(ctx)
}
