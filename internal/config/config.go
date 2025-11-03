package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Role identifies the runtime mode for the proxy service.
type Role string

const (
	RoleProvider Role = "provider"
	RoleMember   Role = "member"
)

const (
	defaultListenAddr          = ":8080"
	defaultRequestTimeout      = 6 * time.Second
	defaultTransportTimeout    = 15 * time.Second
	defaultDialTimeout         = 750 * time.Millisecond
	defaultIdleConnTimeout     = 90 * time.Second
	defaultMaxIdleConns        = 512
	defaultMaxIdleConnsPerHost = 256
	defaultBackgroundRefresh   = 5 * time.Hour
	defaultCacheTTL            = 30 * 24 * time.Hour
)

// Config aggregates runtime configuration derived from environment variables.
type Config struct {
	Role                   Role
	ListenAddr             string
	ProviderClusters       []string
	MemberClusters         []string
	RedisURL               string
	RequestTimeout         time.Duration
	TransportTimeout       time.Duration
	DialTimeout            time.Duration
	IdleConnTimeout        time.Duration
	MaxIdleConns           int
	MaxIdleConnsPerHost    int
	BackgroundRefreshAfter time.Duration
	CacheTTL               time.Duration
	DiscordWebhookURL      string
}

// Load parses environment variables and returns a validated Config.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:             stringOrDefault(os.Getenv("PROXY_LISTEN_ADDR"), defaultListenAddr),
		RequestTimeout:         durationOrDefault(os.Getenv("PROXY_REQUEST_TIMEOUT"), defaultRequestTimeout),
		TransportTimeout:       durationOrDefault(os.Getenv("PROXY_TRANSPORT_TIMEOUT"), defaultTransportTimeout),
		DialTimeout:            durationOrDefault(os.Getenv("PROXY_DIAL_TIMEOUT"), defaultDialTimeout),
		IdleConnTimeout:        durationOrDefault(os.Getenv("PROXY_IDLE_CONN_TIMEOUT"), defaultIdleConnTimeout),
		MaxIdleConns:           intOrDefault(os.Getenv("PROXY_MAX_IDLE_CONNS"), defaultMaxIdleConns),
		MaxIdleConnsPerHost:    intOrDefault(os.Getenv("PROXY_MAX_IDLE_CONNS_PER_HOST"), defaultMaxIdleConnsPerHost),
		BackgroundRefreshAfter: durationOrDefault(os.Getenv("PROXY_BACKGROUND_REFRESH_AFTER"), defaultBackgroundRefresh),
		CacheTTL:               durationOrDefault(os.Getenv("PROXY_CACHE_TTL"), defaultCacheTTL),
		DiscordWebhookURL:      strings.TrimSpace(os.Getenv("PROXY_DISCORD_WEBHOOK_URL")),
	}

	roleRaw := strings.TrimSpace(strings.ToLower(os.Getenv("PROXY_ROLE")))
	switch Role(roleRaw) {
	case RoleProvider:
		cfg.Role = RoleProvider
	case RoleMember:
		cfg.Role = RoleMember
	default:
		return Config{}, fmt.Errorf("invalid PROXY_ROLE %q: must be %q or %q", roleRaw, RoleProvider, RoleMember)
	}

	cfg.RedisURL = strings.TrimSpace(os.Getenv("PROXY_REDIS_URL"))
	if cfg.RedisURL == "" {
		return Config{}, errors.New("PROXY_REDIS_URL must be provided")
	}

	switch cfg.Role {
	case RoleProvider:
		cfg.ProviderClusters = splitAndClean(os.Getenv("PROXY_PROVIDER_CLUSTERS"))
		if len(cfg.ProviderClusters) == 0 {
			return Config{}, errors.New("PROXY_PROVIDER_CLUSTERS must list at least one upstream")
		}
	case RoleMember:
		cfg.MemberClusters = splitAndClean(os.Getenv("PROXY_MEMBER_CLUSTERS"))
		if len(cfg.MemberClusters) == 0 {
			return Config{}, errors.New("PROXY_MEMBER_CLUSTERS must list at least one upstream")
		}
	}

	if cfg.BackgroundRefreshAfter <= 0 {
		return Config{}, errors.New("PROXY_BACKGROUND_REFRESH_AFTER must be positive")
	}

	if cfg.CacheTTL <= 0 {
		return Config{}, errors.New("PROXY_CACHE_TTL must be positive")
	}

	return cfg, nil
}

func stringOrDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func durationOrDefault(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func intOrDefault(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	val, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return val
}

func splitAndClean(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SendDiscordWebhook sends a message to the Discord webhook URL if provided.
func SendDiscordWebhook(webhookURL, message string) {
	if webhookURL == "" {
		return
	}

	payload := map[string]interface{}{
		"username": "RobloxProxyCluster",
		"content":  message,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// Ignore response
}
