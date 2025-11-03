package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	roleProvider Role = "provider"
	roleMember   Role = "member"
)

// Role represents the operating mode for the proxy.
type Role string

// Config captures the runtime configuration sourced from environment variables.
type Config struct {
	Role           Role
	ListenAddr     string
	ClusterTargets []*url.URL
	RedisURL       string
	RequestTimeout time.Duration
}

const (
	defaultListenAddr  = ":8080"
	defaultTimeout     = 3 * time.Second
	providerClusterEnv = "PROXY_PROVIDER_CLUSTERS"
	memberClusterEnv   = "PROXY_MEMBER_CLUSTERS"
)

// Load reads and validates environment configuration for the service.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:     getEnv("PROXY_LISTEN_ADDR", defaultListenAddr),
		RedisURL:       strings.TrimSpace(os.Getenv("PROXY_REDIS_URL")),
		RequestTimeout: defaultTimeout,
	}

	role := strings.ToLower(strings.TrimSpace(getEnv("PROXY_ROLE", string(roleProvider))))
	switch Role(role) {
	case roleProvider, roleMember:
		cfg.Role = Role(role)
	default:
		return nil, fmt.Errorf("invalid PROXY_ROLE: %s", role)
	}

	if timeoutStr := strings.TrimSpace(os.Getenv("PROXY_TIMEOUT_MS")); timeoutStr != "" {
		timeout, err := time.ParseDuration(timeoutStr + "ms")
		if err != nil {
			return nil, fmt.Errorf("invalid PROXY_TIMEOUT_MS: %w", err)
		}
		if timeout < 500*time.Millisecond {
			return nil, errors.New("PROXY_TIMEOUT_MS too low; must be >= 500ms")
		}
		cfg.RequestTimeout = timeout
	}

	clusterEnv := providerClusterEnv
	if cfg.Role == roleMember {
		clusterEnv = memberClusterEnv
	}
	rawTargets := strings.TrimSpace(os.Getenv(clusterEnv))
	if rawTargets == "" {
		return nil, fmt.Errorf("%s must be set with at least one upstream", clusterEnv)
	}
	targets, err := parseTargets(rawTargets)
	if err != nil {
		return nil, err
	}
	cfg.ClusterTargets = targets

	return cfg, nil
}

func parseTargets(input string) ([]*url.URL, error) {
	items := strings.Split(input, ",")
	parsed := make([]*url.URL, 0, len(items))
	for _, item := range items {
		candidate := strings.TrimSpace(item)
		if candidate == "" {
			continue
		}
		if !strings.Contains(candidate, "://") {
			candidate = "https://" + candidate
		}
		u, err := url.Parse(candidate)
		if err != nil {
			return nil, fmt.Errorf("invalid cluster target %q: %w", candidate, err)
		}
		if u.Scheme != "https" && u.Scheme != "http" && !isDirectScheme(u.Scheme) {
			return nil, fmt.Errorf("cluster target %q must include http/https or use direct://", candidate)
		}
		if !isDirectScheme(u.Scheme) && u.Host == "" {
			return nil, fmt.Errorf("cluster target %q must include host", candidate)
		}
		parsed = append(parsed, u)
	}

	if len(parsed) == 0 {
		return nil, errors.New("no valid cluster targets provided")
	}

	return parsed, nil
}

func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func isDirectScheme(scheme string) bool {
	return strings.EqualFold(scheme, "direct") || strings.EqualFold(scheme, "origin")
}
