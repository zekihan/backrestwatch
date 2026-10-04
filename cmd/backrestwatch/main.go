package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zekihan/backrestwatch/internal/config"
	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
	"github.com/zekihan/backrestwatch/internal/monitor"
	"github.com/zekihan/backrestwatch/internal/service"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("Backrestwatch stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	configFile := flag.String("config", "/config/config.json", "Validated JSON configuration file")
	apiURL := flag.String("api-url", "", "Kubernetes HTTPS origin; defaults to in-cluster environment")
	tokenFile := flag.String("token-file", "/var/run/secrets/kubernetes.io/serviceaccount/token", "Projected service-account token")
	caFile := flag.String("ca-file", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "Kubernetes CA bundle")
	showVersion := flag.Bool("version", false, "Print version and exit")
	checkConfig := flag.Bool("check-config", false, "Validate configuration and exit without contacting Kubernetes")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	if *checkConfig {
		fmt.Println("Configuration is valid")
		return nil
	}
	if *apiURL == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return fmt.Errorf("set --api-url or the in-cluster Kubernetes service environment")
		}
		*apiURL = "https://" + net.JoinHostPort(host, port)
	}
	api, err := kube.New(*apiURL, *tokenFile, *caFile, version, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	if err != nil {
		return err
	}
	defer api.HTTP.CloseIdleConnections()
	store, err := evidence.Open(cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("Starting backrestwatch", "version", version, "clusters", len(cfg.Clusters), "address", cfg.ListenAddress)
	return service.Serve(ctx, listener, monitor.New(cfg, api, store, logger))
}
