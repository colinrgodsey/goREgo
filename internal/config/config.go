package config

import (
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	ListenAddr           string `mapstructure:"listen_addr"`
	LocalCacheDir        string `mapstructure:"local_cache_dir"`
	LocalCacheMaxSizeGB  int    `mapstructure:"local_cache_max_size_gb"`
	MaxConcurrentActions int    `mapstructure:"max_concurrent_actions"`

	BackingCache BackingCacheConfig `mapstructure:"backing_cache"`

	Cluster ClusterConfig `mapstructure:"cluster"`
}

type BackingCacheConfig struct {
	Target string `mapstructure:"target"`
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
	v.SetDefault("max_concurrent_actions", 32)
	v.SetDefault("cluster.gossip_port", 7946)

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
