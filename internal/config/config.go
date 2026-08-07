// Package config loads the service configuration from the process environment.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the runtime configuration of the service. Every field is sourced
// from the environment so a single binary can be promoted between environments
// without a rebuild.
type Config struct {
	// Host is the interface to bind to. Empty means all interfaces, which is
	// what container runtimes expect.
	Host string
	Port int

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// ShutdownTimeout bounds how long in-flight requests may take to finish
	// after a termination signal is received.
	ShutdownTimeout time.Duration

	LogLevel slog.Level
}

// Addr returns the TCP address the HTTP server listens on.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Load builds a Config from the environment, falling back to defaults for any
// variable that is unset. A variable that is set but unparseable is an error so
// that misconfiguration fails at startup rather than at request time.
func Load() (Config, error) {
	cfg := Config{
		Host:              stringVar("HOST", ""),
		Port:              8080,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ShutdownTimeout:   15 * time.Second,
		LogLevel:          slog.LevelInfo,
	}

	var err error

	// PORT is the variable Cloud Run and most PaaS providers inject.
	if cfg.Port, err = intVar("PORT", cfg.Port); err != nil {
		return Config{}, err
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("PORT: %d out of range", cfg.Port)
	}
	if cfg.ReadHeaderTimeout, err = durationVar("READ_HEADER_TIMEOUT", cfg.ReadHeaderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ReadTimeout, err = durationVar("READ_TIMEOUT", cfg.ReadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = durationVar("WRITE_TIMEOUT", cfg.WriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = durationVar("IDLE_TIMEOUT", cfg.IdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationVar("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = levelVar("LOG_LEVEL", cfg.LogLevel); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func stringVar(key, fallback string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return fallback
}

func intVar(key string, fallback int) (int, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return n, nil
}

func durationVar(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration", key, v)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %q must not be negative", key, v)
	}
	return d, nil
}

func levelVar(key string, fallback slog.Level) (slog.Level, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		return 0, fmt.Errorf("%s: %q is not a log level", key, v)
	}
	return level, nil
}

func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}
