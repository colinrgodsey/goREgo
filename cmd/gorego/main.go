package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/colinrgodsey/goREgo/internal/config"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	fmt.Printf("Starting goREgo with config: %+v\n", cfg)
}
