// Package config reads settings from environment variables.
package config

import (
	"errors"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr         string
	DatabaseURL  string
	RedisURL     string
	InstanceID   string        // shown in logs and to connected clients
	Secret       []byte        // signs access tokens
	CookieSecure bool          // set COOKIE_SECURE=true when served over HTTPS
	Heartbeat    time.Duration // how often sockets get a heartbeat
	DrainDelay   time.Duration // how long clients get to move away on shutdown
}

func Load() (Config, error) {
	host, _ := os.Hostname()
	cfg := Config{
		Addr:         env("LISTEN_ADDR", ":8080"),
		DatabaseURL:  env("DATABASE_URL", "postgres://chattie:chattie@localhost:5432/chattie"),
		RedisURL:     env("REDIS_URL", "redis://localhost:6379"),
		InstanceID:   env("INSTANCE_ID", host),
		Secret:       []byte(os.Getenv("SESSION_SECRET")),
		CookieSecure: os.Getenv("COOKIE_SECURE") == "true",
		Heartbeat:    seconds("HEARTBEAT_SECONDS", 20),
		DrainDelay:   seconds("DRAIN_SECONDS", 3),
	}
	if len(cfg.Secret) < 32 {
		return cfg, errors.New("SESSION_SECRET must be at least 32 characters")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func seconds(key string, fallback int) time.Duration {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n <= 0 {
		n = fallback
	}
	return time.Duration(n) * time.Second
}
