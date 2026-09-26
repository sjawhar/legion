// packages/envoy/cmd/broker/main.go
package main

import (
	"fmt"
	"os"

	"github.com/sjawhar/envoy/internal/broker/config"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "broker:", err)
		os.Exit(2)
	}
	fmt.Println("broker: configured; listening on", cfg.ListenAddr)
}
