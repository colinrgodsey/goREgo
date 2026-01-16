package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/lib/config"
	"github.com/colinrgodsey/goREgo/lib/janitor"
	"github.com/colinrgodsey/goREgo/lib/proxy"
	"github.com/colinrgodsey/goREgo/lib/server"
	"github.com/colinrgodsey/goREgo/lib/storage"
	"google.golang.org/grpc"
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

	// 1. Storage (Local Tier 1)
	localStore, err := storage.NewLocalStore(cfg.LocalCacheDir, cfg.ForceUpdateATime)
	if err != nil {
		log.Fatalf("failed to initialize local store: %v", err)
	}

	// 2. Janitor
	janitor := janitor.NewJanitor(cfg)
	go func() {
		if err := janitor.Run(ctx); err != nil {
			log.Printf("janitor stopped: %v", err)
		}
	}()

	// 3. Remote Storage (Tier 2 - Optional/Placeholder for now)
	// For Phase 1, if no backing cache is configured, we can just use the local store
	// effectively acting as a single-tier cache, or we could stub it.
	// The ProxyStore requires a remote store.
	// TODO: Implement a proper gRPC client for the backing cache.
	// For now, we will fail if backing cache is accessed, or we could wrap the local store?
	// The requirement is "Hard dependency on backing cache".
	// We will create a stub for now if not configured, or fail.
	
	// Assuming Tier 2 is another CAS/AC service.
	// For this phase, lets just implement the gRPC server and wire it to local for testing
	// if we don't have a backend. But the plan says "Proxy".
	// Let's assume we might point it to `bazel-remote` or similar.
	
	var remoteStore storage.BlobStore // This needs to be a gRPC client wrapper
	// For now, we'll initialize ProxyStore with nil remote and fix it if needed,
	// or assume the user provides a target.
	
	// Real implementation would connect to cfg.BackingCache.Target
	// Since we don't have the client implementation yet, let's defer that
	// and just use local store directly for the server to verify functionality first,
	// OR implement a dummy remote.
	
	// Let's create the ProxyStore.
	// NOTE: We haven't implemented the gRPC Client for storage.BlobStore yet.
	// To unblock, we will use the LocalStore as the "Remote" as well if target is empty,
	// effectively making it a single tier, but exercising the Proxy logic.
	
	if cfg.BackingCache.Target == "" {
		log.Println("WARNING: No backing cache configured. Using local store as remote (Passthrough).")
		remoteStore = localStore
	} else {
		// TODO: Dial cfg.BackingCache.Target
		// conn, err := grpc.Dial(cfg.BackingCache.Target, ...)
		// remoteStore = client.New(conn)
		log.Fatalf("Backing cache connection not implemented yet")
	}

	proxyStore := proxy.NewProxyStore(localStore, remoteStore)

	// 4. gRPC Server
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	
	// Register CAS
	casServer := server.NewContentAddressableStorageServer(proxyStore)
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)
	
	// Register Capabilities
	repb.RegisterCapabilitiesServer(grpcServer, server.NewCapabilitiesServer())

	// Register AC
	// acServer := server.NewActionCacheServer(proxyStore)
	// repb.RegisterActionCacheServer(grpcServer, acServer)
	
	// Register Reflection for debugging (grpcurl)
	reflection.Register(grpcServer)

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		grpcServer.GracefulStop()
		cancel()
	}()

	log.Printf("Listening on %s", cfg.ListenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}