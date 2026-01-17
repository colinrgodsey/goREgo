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

	BackingCache BackingCacheConfig `mapstructure:"backing_cache"`
	Telemetry    TelemetryConfig    `mapstructure:"telemetry"`
	Execution    ExecutionConfig    `mapstructure:"execution"`
}

type ExecutionConfig struct {
	Enabled     bool   `mapstructure:"enabled"`
	Concurrency int    `mapstructure:"concurrency"` // 0 = runtime.NumCPU()
	BuildRoot   string `mapstructure:"build_root"`
	QueueSize   int    `mapstructure:"queue_size"`
}

type BackingCacheConfig struct {
	Target string `mapstructure:"target"`
}

type TelemetryConfig struct {
	MetricsAddr     string `mapstructure:"metrics_addr"`
	TracingEndpoint string `mapstructure:"tracing_endpoint"`
}

type ClusterConfig struct {
	JoinPeers  []string `mapstructure:"join_peers"`
	GossipPort int      `mapstructure:"gossip_port"`
}

func LoadConfig(configPath string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("listen_addr", ":50051")
	v.SetDefault("local_cache_dir", "/var/lib/gorego/cache")
	v.SetDefault("local_cache_max_size_gb", 100)
	v.SetDefault("cluster.gossip_port", 7946)
	v.SetDefault("force_update_atime", false)
	v.SetDefault("telemetry.metrics_addr", ":9090")
	v.SetDefault("execution.enabled", false)
	v.SetDefault("execution.concurrency", 0)
	v.SetDefault("execution.build_root", "/tmp/gorego/builds")
	v.SetDefault("execution.queue_size", 1000)

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
