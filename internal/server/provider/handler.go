package provider

import (
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"

	"github.com/NoahCxrest/roblox-proxy-clustering/internal/config"
	"github.com/NoahCxrest/roblox-proxy-clustering/internal/proxy"
	"github.com/NoahCxrest/roblox-proxy-clustering/internal/upstream"
)

const (
	headerContentType              = "Content-Type"
	headerAccessControlAllowOrigin = "Access-Control-Allow-Origin"
	contentTypeJSON                = "application/json"
)

// Handler proxies provider traffic to member clusters.
type Handler struct {
	cfg       config.Config
	logger    *slog.Logger
	forwarder *proxy.Forwarder
	upstreams []*url.URL
}

// New constructs a provider handler.
func New(cfg config.Config, logger *slog.Logger, client *http.Client) (*Handler, error) {
	upstreams, err := upstream.ParseProviderTargets(cfg.ProviderClusters)
	if err != nil {
		return nil, err
	}

	return &Handler{
		cfg:    cfg,
		logger: logger.With(slog.String("component", "provider-handler")),
		forwarder: &proxy.Forwarder{
			Client:            client,
			Logger:            logger,
			RequestTimeout:    cfg.RequestTimeout,
			DiscordWebhookURL: cfg.DiscordWebhookURL,
		},
		upstreams: upstreams,
	}, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := h.pickTarget(r)
	if err != nil {
		h.respondError(w, http.StatusBadGateway, err)
		return
	}

	if err := h.forwarder.Do(w, r, target); err != nil {
		h.logger.Error("provider forward failed", slog.String("target", target.Host), slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		h.respondError(w, http.StatusBadGateway, err)
	}
}

func (h *Handler) pickTarget(r *http.Request) (*url.URL, error) {
	if len(h.upstreams) == 0 {
		return nil, fmt.Errorf("no provider upstreams configured")
	}

	key := r.URL.Path
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}

	idx := rand.Intn(len(h.upstreams))
	base := h.upstreams[idx]
	rel := &url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	return base.ResolveReference(rel), nil
}

func (h *Handler) respondError(w http.ResponseWriter, status int, err error) {
	msg := fmt.Sprintf(`{"error":"%s"}`, sanitize(err))
	w.Header().Set(headerContentType, contentTypeJSON)
	w.Header().Set(headerAccessControlAllowOrigin, "*")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), "\"", "'")
}
