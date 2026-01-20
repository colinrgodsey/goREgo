package config

import (
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	ListenAddr          string `mapstructure:"listen_addr"`
	LocalCacheDir       string `mapstructure:"local_cache_dir"`
	LocalCacheMaxSizeGB int    `mapstructure:"local_cache_max_size_gb"`
	ForceUpdateATime    bool   `mapstructure:"force_update_atime"`
	LogLevel            string `mapstructure:"log_level"`

	BackingCache BackingCacheConfig `mapstructure:"backing_cache"`
	Telemetry    TelemetryConfig    `mapstructure:"telemetry"`
	Execution    ExecutionConfig    `mapstructure:"execution"`
	Cluster      ClusterConfig      `mapstructure:"cluster"`
}

type ExecutionConfig struct {
	Enabled     bool          `mapstructure:"enabled"`
	Concurrency int           `mapstructure:"concurrency"` // 0 = runtime.NumCPU()
	BuildRoot   string        `mapstructure:"build_root"`
	QueueSize   int           `mapstructure:"queue_size"`
	Sandbox     SandboxConfig `mapstructure:"sandbox"`
}

type SandboxConfig struct {
	Enabled          bool     `mapstructure:"enabled"`
	BinaryPath       string   `mapstructure:"binary_path"`
	NetworkIsolation bool     `mapstructure:"network_isolation"`
	WritablePaths    []string `mapstructure:"writable_paths"`
	KillDelay        int      `mapstructure:"kill_delay"` // seconds after timeout to SIGKILL
	Debug            bool     `mapstructure:"debug"`
}

type BackingCacheConfig struct {
	Target        string `mapstructure:"target"`
	Compression   string `mapstructure:"compression"`     // "zstd" or empty
	PutRetryCount int    `mapstructure:"put_retry_count"` // Number of retries for remote put operations
}

type TelemetryConfig struct {
	MetricsAddr     string `mapstructure:"metrics_addr"`
	TracingEndpoint string `mapstructure:"tracing_endpoint"`
}

// ClusterConfig holds configuration for the distributed cluster mesh.
type ClusterConfig struct {
	Enabled        bool     `mapstructure:"enabled"`
	NodeID         string   `mapstructure:"node_id"`          // Unique node identifier (auto-generated if empty)
	BindPort       int      `mapstructure:"bind_port"`        // Gossip port for memberlist (default: 7946)
	AdvertiseAddr  string   `mapstructure:"advertise_addr"`   // Address to advertise to peers (auto-detect if empty)
	DiscoveryMode  string   `mapstructure:"discovery_mode"`   // "list" or "dns"
	JoinPeers      []string `mapstructure:"join_peers"`       // Static peer addresses (used when discovery_mode == "list")
	DNSServiceName string   `mapstructure:"dns_service_name"` // DNS hostname to resolve (used when discovery_mode == "dns")
}

func LoadConfig(configPath string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("listen_addr", ":50051")
	v.SetDefault("local_cache_dir", "/tmp/gorego/cache")
	v.SetDefault("local_cache_max_size_gb", 100)
	v.SetDefault("cluster.enabled", false)
	v.SetDefault("cluster.bind_port", 7946)
	v.SetDefault("cluster.discovery_mode", "list")
	v.SetDefault("force_update_atime", false)
	v.SetDefault("log_level", "warn")
	v.SetDefault("telemetry.metrics_addr", ":9090")
	v.SetDefault("execution.enabled", true)
	v.SetDefault("execution.concurrency", 0)
	v.SetDefault("execution.build_root", "/tmp/gorego/builds")
	v.SetDefault("execution.queue_size", 1000)
	v.SetDefault("execution.sandbox.enabled", false)
	v.SetDefault("execution.sandbox.binary_path", "/usr/bin/linux-sandbox")
	v.SetDefault("execution.sandbox.network_isolation", true)
	v.SetDefault("execution.sandbox.writable_paths", []string{})
	v.SetDefault("execution.sandbox.kill_delay", 5)
	v.SetDefault("execution.sandbox.debug", false)
	v.SetDefault("backing_cache.put_retry_count", 3)

	// Env overrides
	v.SetEnvPrefix("GOREGO")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Config file
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./config")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
