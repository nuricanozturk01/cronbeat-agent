// Command agent is the CronBeat Kubernetes agent. It watches CronJobs, Jobs and
// Pods (read-only) and reports them to the CronBeat API over outbound HTTPS.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"cronbeat.dev/cronbeat-agent/internal/api"
	"cronbeat.dev/cronbeat-agent/internal/app"
	"cronbeat.dev/cronbeat-agent/internal/config"
	"cronbeat.dev/cronbeat-agent/internal/health"
	"cronbeat.dev/cronbeat-agent/internal/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if err := run(); err != nil {
		slog.Error("agent stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("starting cronbeat-agent",
		"version", version.Version, "cluster", cfg.ClusterName, "apiUrl", cfg.APIURL,
		"watchNamespaces", cfg.WatchNamespaces)

	restCfg, err := kubeConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	probes := health.New(health.DefaultMaxSilence)
	if cfg.HealthAddr != "" {
		lis, err := net.Listen("tcp", cfg.HealthAddr)
		if err != nil {
			return fmt.Errorf("health listener: %w", err)
		}
		go func() {
			if err := health.Serve(ctx, lis, probes.Handler()); err != nil {
				log.Error("health server stopped", "error", err.Error())
			}
		}()
	}

	apiClient := api.NewClient(cfg.APIURL, cfg.APIKey, nil, api.DefaultRetry(), log,
		api.WithUserAgent("cronbeat-agent/"+version.Version))
	return app.Run(ctx, app.Options{
		Config: cfg, Kube: client, API: apiClient, Log: log, Health: probes, Version: version.Version,
	})
}

// kubeConfig uses the in-cluster config; KUBECONFIG is honoured for local runs.
func kubeConfig() (*rest.Config, error) {
	if path := os.Getenv("KUBECONFIG"); path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return rest.InClusterConfig()
}
