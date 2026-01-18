package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/colinrgodsey/goREgo/pkg/execution"
	"github.com/colinrgodsey/goREgo/pkg/janitor"
	"github.com/colinrgodsey/goREgo/pkg/proxy"
	"github.com/colinrgodsey/goREgo/pkg/scheduler"
	"github.com/colinrgodsey/goREgo/pkg/server"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"github.com/colinrgodsey/goREgo/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Configure structured logging
	logLevel := parseLogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	slog.Info("starting goREgo", "log_level", cfg.LogLevel, "listen_addr", cfg.ListenAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	// 0. Telemetry
	reg, shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Config{
		MetricsAddr:     cfg.Telemetry.MetricsAddr,
		TracingEndpoint: cfg.Telemetry.TracingEndpoint,
	})
	if err != nil {
		slog.Error("failed to setup telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		// Shutdown telemetry last
		if err := shutdownTelemetry(context.Background()); err != nil {
			slog.Error("telemetry shutdown failed", "error", err)
		}
	}()

	// Start Metrics & pprof Server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	// pprof handlers
	metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
	metricsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	metricsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	metricsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	metricsServer := &http.Server{
		Addr:    cfg.Telemetry.MetricsAddr,
		Handler: metricsMux,
	}

	g.Go(func() error {
		slog.Info("Serving metrics and pprof", "addr", cfg.Telemetry.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return metricsServer.Shutdown(shutdownCtx)
	})

	// 1. Storage (Local Tier 1)
	localStore, err := storage.NewLocalStore(cfg.LocalCacheDir, cfg.ForceUpdateATime)
	if err != nil {
		slog.Error("failed to initialize local store", "error", err)
		os.Exit(1)
	}

	// 2. Janitor
	janitor := janitor.NewJanitor(cfg)
	g.Go(func() error {
		if err := janitor.Run(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				return err
			}
		}
		return nil
	})

	// 3. Remote Storage (Tier 2 - Optional/Placeholder for now)
	var remoteStore storage.BlobStore
	if cfg.BackingCache.Target == "" {
		slog.Warn("No backing cache configured. Using local store as remote (Passthrough).")
		remoteStore = localStore
	} else {
		slog.Info("Connecting to backing cache", "target", cfg.BackingCache.Target)
		var err error
		remoteStore, err = storage.NewRemoteStore(ctx, cfg.BackingCache.Target)
		if err != nil {
			slog.Error("failed to connect to backing cache", "error", err)
			os.Exit(1)
		}
	}

	proxyStore := proxy.NewProxyStore(localStore, remoteStore)

	// 3.5 Scheduler and Execution (if enabled)
	var sched *scheduler.Scheduler
	if cfg.Execution.Enabled {
		sched = scheduler.NewScheduler(cfg.Execution.QueueSize)

		// Start scheduler cleanup goroutine
		g.Go(func() error {
			return sched.Run(ctx)
		})

		// Start worker pool
		workerPool, err := execution.NewWorkerPool(cfg.Execution, sched, proxyStore, proxyStore)
		if err != nil {
			slog.Error("failed to initialize worker pool", "error", err)
			os.Exit(1)
		}
		g.Go(func() error {
			if err := workerPool.Run(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					return err
				}
			}
			return nil
		})

		slog.Info("Execution enabled", "concurrency", cfg.Execution.Concurrency, "build_root", cfg.Execution.BuildRoot)
	}

	// 4. gRPC Server
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		telemetry.NewServerHandler(),
	)

	// Register CAS
	casServer := server.NewContentAddressableStorageServer(proxyStore)
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)

	// Register Capabilities
	repb.RegisterCapabilitiesServer(grpcServer, server.NewCapabilitiesServer(cfg.Execution.Enabled))

	// Register ByteStream
	bytestream.RegisterByteStreamServer(grpcServer, server.NewByteStreamServer(proxyStore))

	// Register AC
	repb.RegisterActionCacheServer(grpcServer, server.NewActionCacheServer(proxyStore))

	// Register Execution and Operations (if enabled)
	if cfg.Execution.Enabled && sched != nil {
		repb.RegisterExecutionServer(grpcServer, server.NewExecutionServer(sched, proxyStore))
		longrunning.RegisterOperationsServer(grpcServer, server.NewOperationsServer(sched))
	}

	// Register Health
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	// Mark as serving
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register Reflection for debugging (grpcurl)
	reflection.Register(grpcServer)

	g.Go(func() error {
		slog.Info("gRPC server listening", "addr", cfg.ListenAddr)
		return grpcServer.Serve(lis)
	})

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		slog.Info("Shutting down...")
		// 1. Mark unhealthy
		healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

		// 2. Stop gRPC (blocks)
		grpcServer.GracefulStop()

		// 3. Stop other background tasks
		cancel()
	case <-ctx.Done():
		// Error in group
	}

	if err := g.Wait(); err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("Server error", "error", err)
		}
	}
}
