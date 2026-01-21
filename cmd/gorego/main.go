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
	"runtime"
	"strings"
	"syscall"
	"time"

	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/cluster"
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

	// 1. Storage
	var store storage.Store
	var localStore *storage.LocalStore

	if cfg.LocalCache.Enabled {
		var err error
		localStore, err = storage.NewLocalStore(cfg.LocalCache.Dir, cfg.ForceUpdateATime)
		if err != nil {
			slog.Error("failed to initialize local store", "error", err)
			os.Exit(1)
		}

		// 2. Janitor (only needed if local cache is enabled)
		g.Go(func() error {
			if err := janitor.NewJanitor(cfg).Run(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					return err
				}
			}
			return nil
		})

		// 3. Remote Storage (Tier 2)
		var remoteStore storage.Store
		if cfg.BackingCache.Target == "" {
			slog.Warn("No backing cache configured. Using null store as remote.")
			remoteStore = storage.NewNullStore()
		} else {
			slog.Info("Connecting to backing cache", "target", cfg.BackingCache.Target, "compression", cfg.BackingCache.Compression)
			var err error
			remoteStore, err = storage.NewRemoteStore(ctx, cfg.BackingCache.Target, cfg.BackingCache.Compression)
			if err != nil {
				slog.Error("failed to connect to backing cache", "error", err)
				os.Exit(1)
			}
		}

		// Use ProxyStore (Local + Remote)
		store = proxy.NewProxyStore(localStore, remoteStore, cfg.BackingCache.PutRetryCount)
	} else {
		// Local cache disabled - "Stateless" mode
		if cfg.BackingCache.Target == "" {
			slog.Error("Local cache disabled but no backing cache configured. A backing cache is required for stateless mode.")
			os.Exit(1)
		}

		slog.Info("Local cache disabled. Running in stateless mode.", "backing_cache", cfg.BackingCache.Target)
		var err error
		store, err = storage.NewRemoteStore(ctx, cfg.BackingCache.Target, cfg.BackingCache.Compression)
		if err != nil {
			slog.Error("failed to connect to backing cache", "error", err)
			os.Exit(1)
		}
	}

	// 3.5 Cluster Manager (if enabled)
	var clusterManager *cluster.Manager
	if cfg.Cluster.Enabled {
		concurrency := cfg.Execution.Concurrency
		if concurrency == 0 {
			concurrency = runtime.NumCPU()
		}

		var err error
		clusterManager, err = cluster.NewManager(cfg.Cluster, cfg.ListenAddr, concurrency, nil, logger)
		if err != nil {
			slog.Error("failed to create cluster manager", "error", err)
			os.Exit(1)
		}

		if err := clusterManager.Start(ctx); err != nil {
			slog.Error("failed to start cluster", "error", err)
			os.Exit(1)
		}

		// Run periodic state broadcasts
		g.Go(func() error {
			if err := clusterManager.Run(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					return err
				}
			}
			return nil
		})

		slog.Info("Cluster enabled",
			"node_id", clusterManager.NodeID(),
			"bind_port", cfg.Cluster.BindPort,
			"discovery_mode", cfg.Cluster.DiscoveryMode,
		)
	}

	// 3.6 Scheduler and Execution (if enabled)
	var sched *scheduler.Scheduler
	if cfg.Execution.Enabled {
		nodeID := ""
		if clusterManager != nil {
			nodeID = clusterManager.NodeID()
		}
		sched = scheduler.NewScheduler(cfg.Execution.QueueSize, nodeID)

		// Wire up load provider for cluster
		if clusterManager != nil {
			clusterManager.SetLoadProvider(sched)
		}

		// Start scheduler cleanup goroutine
		g.Go(func() error {
			return sched.Run(ctx)
		})

		workerPool, err := execution.NewWorkerPool(cfg.Execution, sched, store, store)
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
	casServer := server.NewContentAddressableStorageServer(store)
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)

	// Register Capabilities
	repb.RegisterCapabilitiesServer(grpcServer, server.NewCapabilitiesServer(cfg.Execution.Enabled))

	// Register ByteStream
	bytestream.RegisterByteStreamServer(grpcServer, server.NewByteStreamServer(store))

	// Register AC
	repb.RegisterActionCacheServer(grpcServer, server.NewActionCacheServer(store))

	// Register Execution and Operations (if enabled)
	if cfg.Execution.Enabled && sched != nil {
		repb.RegisterExecutionServer(grpcServer, server.NewExecutionServer(sched, store, clusterManager))
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

		// 2. Leave cluster gracefully
		if clusterManager != nil {
			if err := clusterManager.Stop(5 * time.Second); err != nil {
				slog.Error("failed to leave cluster", "error", err)
			}
		}

		// 3. Stop gRPC (blocks)
		grpcServer.GracefulStop()

		// 4. Stop other background tasks
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
