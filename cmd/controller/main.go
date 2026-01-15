package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/WD-Mitchell/omni-autoscaler/internal/config"
	"github.com/WD-Mitchell/omni-autoscaler/internal/controller"
	"github.com/WD-Mitchell/omni-autoscaler/internal/omni"
)

func main() {
	// Parse flags
	configPath := flag.String("config", "/etc/omni-autoscaler/config.yaml", "Path to configuration file")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	healthAddr := flag.String("health-addr", ":8080", "Health check server address")
	flag.Parse()

	// Setup logger
	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("Starting omni-autoscaler")

	// Start health server
	healthServer := controller.NewHealthServer(*healthAddr)
	go func() {
		logger.Info("Starting health server", "addr", *healthAddr)
		if err := healthServer.Start(); err != nil && err != http.ErrServerClosed {
			logger.Error("Health server error", "error", err)
		}
	}()

	// Load configuration
	cfg, err := config.LoadFromFile(*configPath)
	if err != nil {
		logger.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	logger.Info("Configuration loaded",
		"clusterName", cfg.ClusterName,
		"machineSets", len(cfg.MachineSets),
	)

	// Create Kubernetes client (in-cluster config)
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("Failed to get in-cluster config", "error", err)
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	// Create Omni client
	omniClient, err := omni.NewClient(cfg.OmniEndpoint, cfg.ClusterName)
	if err != nil {
		logger.Error("Failed to create Omni client", "error", err)
		os.Exit(1)
	}
	defer omniClient.Close()

	// Create autoscaler
	autoscaler := controller.NewAutoscaler(kubeClient, omniClient, cfg, logger)

	// Mark as ready
	healthServer.SetReady(true)

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("Received signal, shutting down", "signal", sig)
		healthServer.SetReady(false)
		cancel()
	}()

	// Run the autoscaler
	if err := autoscaler.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("Autoscaler error", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown of health server
	healthServer.Shutdown(context.Background())

	logger.Info("Shutdown complete")
}
