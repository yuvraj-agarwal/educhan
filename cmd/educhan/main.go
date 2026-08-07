// Command educhan runs the educhan HTTP service.
package main

import (
	"log"
	"os"

	"github.com/yuvraj-agarwal/educhan/internal/config"
	"github.com/yuvraj-agarwal/educhan/internal/server"
)

func main() {
	cfg := config.Load()

	srv := server.New(cfg)

	log.Printf("educhan starting on %s", cfg.Addr)
	if err := srv.Start(); err != nil {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}
