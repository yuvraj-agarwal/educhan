// Package config loads the service configuration from the process environment.
package config

import "os"

// Config is the runtime configuration of the service. Values come from the
// environment so a single binary can be promoted between environments without
// a rebuild.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string
	// Env names the deployment environment, such as development or production.
	Env string
}

// Load reads configuration from the environment, falling back to
// development-friendly defaults for anything unset.
func Load() Config {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	env := os.Getenv("ENV")
	if env == "" {
		env = "development"
	}

	return Config{
		Addr: addr,
		Env:  env,
	}
}
