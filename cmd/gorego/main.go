package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/colinrgodsey/goREgo/pkg/janitor"
	"github.com/colinrgodsey/goREgo/pkg/proxy"
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

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("Starting goREgo with config: %+v", cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	// 0. Telemetry
	reg, shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Config{
		MetricsAddr:     cfg.Telemetry.MetricsAddr,
		TracingEndpoint: cfg.Telemetry.TracingEndpoint,
	})
	if err != nil {
		log.Fatalf("failed to setup telemetry: %v", err)
	}
	defer func() {
		// Shutdown telemetry last
		if err := shutdownTelemetry(context.Background()); err != nil {
			log.Printf("telemetry shutdown failed: %v", err)
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
		log.Printf("Serving metrics and pprof on %s", cfg.Telemetry.MetricsAddr)
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
		log.Fatalf("failed to initialize local store: %v", err)
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
		log.Println("WARNING: No backing cache configured. Using local store as remote (Passthrough).")
		remoteStore = localStore
	} else {
		log.Printf("Connecting to backing cache at %s", cfg.BackingCache.Target)
		var err error
		remoteStore, err = storage.NewRemoteStore(ctx, cfg.BackingCache.Target)
		if err != nil {
			log.Fatalf("failed to connect to backing cache: %v", err)
		}
	}

	proxyStore := proxy.NewProxyStore(localStore, remoteStore)

	// 4. gRPC Server
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer(
		telemetry.NewServerHandler(),
	)

	// Register CAS
	casServer := server.NewContentAddressableStorageServer(proxyStore)
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)

	// Register Capabilities
	repb.RegisterCapabilitiesServer(grpcServer, server.NewCapabilitiesServer())

	// Register ByteStream
	bytestream.RegisterByteStreamServer(grpcServer, server.NewByteStreamServer(proxyStore))

	// Register AC
	repb.RegisterActionCacheServer(grpcServer, server.NewActionCacheServer(proxyStore))

	// Register Health
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	// Mark as serving
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register Reflection for debugging (grpcurl)
	reflection.Register(grpcServer)

	g.Go(func() error {
		log.Printf("Listening on %s", cfg.ListenAddr)
		return grpcServer.Serve(lis)
	})

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	
	select {
	case <-sigChan:
		log.Println("Shutting down...")
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
			log.Printf("Server error: %v", err)
		}
	}
}
