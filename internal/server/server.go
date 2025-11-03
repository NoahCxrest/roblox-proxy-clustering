package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"roblox-proxy-clustering/internal/cache"
	"roblox-proxy-clustering/internal/config"
	"roblox-proxy-clustering/internal/httpclient"
)

const (
	userProfileCacheTTLSeconds = 18000 // 5 hours
	searchCacheTTLSeconds      = 3600  // 1 hour
	avatarCacheTTLSeconds      = 3600
	maxProxyBodyBytes          = 4 << 20 // 4 MiB
)

var (
	domainSegmentPattern = regexp.MustCompile(`^[a-z0-9-]+$`)
	userIDPattern        = regexp.MustCompile(`^\d+$`)
)

// Server provides the HTTP entrypoint for the Roblox proxy cluster.
type Server struct {
	cfg    *config.Config
	client *httpclient.Client
	cache  cache.Layer
	logger *slog.Logger
}

// New constructs a server instance.
func New(cfg *config.Config, client *httpclient.Client, cache cache.Layer, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, client: client, cache: cache, logger: logger}
}

// Handler exposes the server as an http.Handler implementation.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.ServeHTTP)
}

// ServeHTTP routes the request to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		s.handleOptions(w)
		return
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		// continue
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	query := r.URL.Query()
	if userID := query.Get("userId"); userID != "" {
		s.handleUserProfile(w, r, userID)
		return
	}

	if search := query.Get("search"); search != "" {
		s.handleSearch(w, r, search)
		return
	}

	s.handleTransparentProxy(w, r)
}

func (s *Server) handleOptions(w http.ResponseWriter) {
	setCORSHeaders(w.Header())
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,POST,PUT,PATCH,DELETE,OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUserProfile(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if !userIDPattern.MatchString(userID) {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid or missing userId")
		return
	}

	ctx := r.Context()
	cacheKey := "roblox:user:" + userID
	if cached, err := s.cache.Get(ctx, cacheKey); err == nil && len(cached) > 0 {
		s.writeJSONBytes(w, http.StatusOK, cached, userProfileCacheTTLSeconds)
		return
	} else if err != nil {
		s.logger.WarnContext(ctx, "cache lookup failed", slog.String("key", cacheKey), slog.Any("err", err))
	}

	var userResp robloxUserResponse
	if err := s.client.FetchJSON(ctx, "users.roblox.com", fmt.Sprintf("/v1/users/%s", userID), nil, &userResp); err != nil {
		s.logger.ErrorContext(ctx, "user lookup failed", slog.String("userId", userID), slog.Any("err", err))
		s.writeJSONError(w, upstreamStatus(err), "Failed to fetch user information")
		return
	}

	avatarURL, err := s.fetchAvatarURL(ctx, userID)
	if err != nil {
		s.logger.WarnContext(ctx, "avatar fetch failed", slog.String("userId", userID), slog.Any("err", err))
		avatarURL = ""
	}

	combined := combinedUserResponse{
		Description: userResp.Description,
		Created:     userResp.Created,
		IsBanned:    userResp.IsBanned,
		ID:          userResp.ID,
		Name:        userResp.Name,
		DisplayName: userResp.DisplayName,
		AvatarURL:   avatarURL,
	}

	payload, err := json.Marshal(combined)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to marshal user payload", slog.String("userId", userID), slog.Any("err", err))
		s.writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	if err := s.cache.Set(ctx, cacheKey, payload, userProfileCacheTTLSeconds); err != nil {
		s.logger.WarnContext(ctx, "cache write failed", slog.String("key", cacheKey), slog.Any("err", err))
	}

	s.writeJSONBytes(w, http.StatusOK, payload, userProfileCacheTTLSeconds)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, search string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trimmed := strings.TrimSpace(search)
	if len([]rune(trimmed)) < 3 {
		s.writeJSONBytes(w, http.StatusBadRequest, []byte("[]"), 0)
		return
	}

	ctx := r.Context()
	cacheKey := "roblox:search:" + strings.ToLower(trimmed)
	if cached, err := s.cache.Get(ctx, cacheKey); err == nil && len(cached) > 0 {
		s.writeJSONBytes(w, http.StatusOK, cached, 0)
		return
	} else if err != nil {
		s.logger.WarnContext(ctx, "cache lookup failed", slog.String("key", cacheKey), slog.Any("err", err))
	}

	var searchResp robloxSearchResponse
	query := url.Values{
		"verticalType":    {"user"},
		"searchQuery":     {trimmed},
		"globalSessionId": {"TridentBot"},
		"sessionId":       {"TridentBot"},
	}

	if err := s.client.FetchJSON(ctx, "apis.roblox.com", "/search-api/omni-search", query, &searchResp); err != nil {
		s.logger.ErrorContext(ctx, "search fetch failed", slog.String("search", trimmed), slog.Any("err", err))
		s.writeJSONError(w, upstreamStatus(err), "Failed to fetch data from Roblox API")
		return
	}

	contents := searchResp.FirstContents()
	if len(contents) == 0 {
		s.writeJSONBytes(w, http.StatusOK, []byte("[]"), 0)
		return
	}

	results := make([]searchResult, len(contents))
	group, groupCtx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, 6)

	for i := range contents {
		i := i
		entry := contents[i]
		group.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-groupCtx.Done():
				return groupCtx.Err()
			}
			defer func() { <-sem }()

			avatarURL, err := s.fetchAvatarURL(groupCtx, strconv.FormatInt(entry.ContentID, 10))
			if err != nil {
				s.logger.WarnContext(groupCtx, "avatar fetch failed", slog.Int64("contentId", entry.ContentID), slog.Any("err", err))
				avatarURL = ""
			}

			results[i] = searchResult{
				PlayerID:  strconv.FormatInt(entry.ContentID, 10),
				Name:      entry.Username,
				AvatarURL: avatarURL,
			}
			return nil
		})
	}

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.ErrorContext(ctx, "search assembly failed", slog.String("search", trimmed), slog.Any("err", err))
		s.writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	payload, err := json.Marshal(results)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to marshal search payload", slog.String("search", trimmed), slog.Any("err", err))
		s.writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	if err := s.cache.Set(ctx, cacheKey, payload, searchCacheTTLSeconds); err != nil {
		s.logger.WarnContext(ctx, "cache write failed", slog.String("key", cacheKey), slog.Any("err", err))
	}

	s.writeJSONBytes(w, http.StatusOK, payload, 0)
}

func (s *Server) handleTransparentProxy(w http.ResponseWriter, r *http.Request) {
	host, path, err := deriveRobloxTarget(r.URL.Path)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	var bodyReader io.Reader
	var contentLength int64 = -1

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		buf, err := readRequestBody(r)
		if err != nil {
			if errors.Is(err, errBodyTooLarge) {
				s.writeJSONError(w, http.StatusRequestEntityTooLarge, "Request body too large")
				return
			}
			s.logger.Error("failed to read request body", slog.Any("err", err))
			s.writeJSONError(w, http.StatusBadGateway, "Failed to read request body")
			return
		}
		bodyReader = bytes.NewReader(buf)
		contentLength = int64(len(buf))
	}

	resp, err := s.client.Forward(r.Context(), &httpclient.ForwardRequest{
		Method:        r.Method,
		RobloxHost:    host,
		Path:          path,
		UpstreamPath:  r.URL.Path,
		RawQuery:      r.URL.RawQuery,
		Header:        r.Header,
		Body:          bodyReader,
		ContentLength: contentLength,
		OriginalHost:  r.Host,
		RemoteAddr:    r.RemoteAddr,
	})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "upstream proxy failure", slog.String("path", r.URL.Path), slog.Any("err", err))
		s.writeJSONError(w, upstreamStatus(err), "Upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if r.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent {
		return
	}

	if _, err := io.CopyBuffer(w, resp.Body, make([]byte, 32*1024)); err != nil {
		s.logger.WarnContext(r.Context(), "error streaming upstream response", slog.Any("err", err))
	}
}

func (s *Server) fetchAvatarURL(ctx context.Context, userID string) (string, error) {
	cacheKey := "roblox:avatar:" + userID
	if cached, err := s.cache.Get(ctx, cacheKey); err == nil && len(cached) > 0 {
		return string(cached), nil
	} else if err != nil {
		s.logger.WarnContext(ctx, "cache lookup failed", slog.String("key", cacheKey), slog.Any("err", err))
	}

	var avatarResp robloxAvatarResponse
	query := url.Values{
		"userIds":    {userID},
		"size":       {"420x420"},
		"format":     {"Png"},
		"isCircular": {"false"},
	}

	if err := s.client.FetchJSON(ctx, "thumbnails.roblox.com", "/v1/users/avatar-bust", query, &avatarResp); err != nil {
		return "", err
	}

	avatarURL := avatarResp.FirstImageURL()
	if avatarURL == "" {
		return "", nil
	}

	if err := s.cache.Set(ctx, cacheKey, []byte(avatarURL), avatarCacheTTLSeconds); err != nil {
		s.logger.WarnContext(ctx, "cache write failed", slog.String("key", cacheKey), slog.Any("err", err))
	}

	return avatarURL, nil
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()

	limited := io.LimitReader(r.Body, maxProxyBodyBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maxProxyBodyBytes {
		return nil, errBodyTooLarge
	}
	return buf, nil
}

var errBodyTooLarge = errors.New("request body too large")

func deriveRobloxTarget(path string) (string, string, error) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "", errors.New("missing Roblox service prefix in path")
	}

	segments := strings.Split(trimmed, "/")
	prefix := segments[0]
	if !domainSegmentPattern.MatchString(prefix) {
		return "", "", fmt.Errorf("invalid Roblox service prefix: %s", prefix)
	}

	for _, seg := range segments[1:] {
		if seg == ".." {
			return "", "", errors.New("path traversal not allowed")
		}
	}

	rest := strings.Join(segments[1:], "/")
	if rest == "" {
		rest = "/"
	} else {
		rest = "/" + rest
	}

	return prefix + ".roblox.com", rest, nil
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		dst[key] = append([]string(nil), values...)
	}
}

func setCORSHeaders(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Credentials", "false")
}

func (s *Server) writeJSONError(w http.ResponseWriter, status int, message string) {
	payload := map[string]string{"error": message}
	data, _ := json.Marshal(payload)
	s.writeJSONBytes(w, status, data, 0)
}

func (s *Server) writeJSONBytes(w http.ResponseWriter, status int, payload []byte, cacheSeconds int) {
	header := w.Header()
	setCORSHeaders(header)
	header.Set("Content-Type", "application/json")
	if cacheSeconds > 0 {
		header.Set("Cache-Control", fmt.Sprintf("max-age=%d", cacheSeconds))
	} else {
		header.Del("Cache-Control")
	}
	w.WriteHeader(status)
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			s.logger.Warn("failed to write response", slog.Any("err", err))
		}
	}
}

func upstreamStatus(err error) int {
	var httpErr *httpclient.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests
		case http.StatusNotFound:
			return http.StatusNotFound
		default:
			if httpErr.StatusCode >= 500 {
				return http.StatusBadGateway
			}
		}
	}
	return http.StatusBadGateway
}

// Internal response shapes ---------------------------------------------------

type combinedUserResponse struct {
	Description string `json:"description"`
	Created     string `json:"created"`
	IsBanned    bool   `json:"isBanned"`
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl"`
}

type robloxUserResponse struct {
	Description string `json:"description"`
	Created     string `json:"created"`
	IsBanned    bool   `json:"isBanned"`
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type robloxAvatarResponse struct {
	Data []struct {
		ImageURL string `json:"imageUrl"`
	} `json:"data"`
}

func (r robloxAvatarResponse) FirstImageURL() string {
	if len(r.Data) == 0 {
		return ""
	}
	return r.Data[0].ImageURL
}

type robloxSearchResponse struct {
	SearchResults []struct {
		Contents []struct {
			ContentID int64  `json:"contentId"`
			Username  string `json:"username"`
		} `json:"contents"`
	} `json:"searchResults"`
}

func (r robloxSearchResponse) FirstContents() []struct {
	ContentID int64
	Username  string
} {
	if len(r.SearchResults) == 0 {
		return nil
	}

	contents := r.SearchResults[0].Contents
	output := make([]struct {
		ContentID int64
		Username  string
	}, len(contents))

	for i, entry := range contents {
		output[i].ContentID = entry.ContentID
		output[i].Username = entry.Username
	}

	return output
}

func (r robloxSearchResponse) Len() int {
	if len(r.SearchResults) == 0 {
		return 0
	}
	return len(r.SearchResults[0].Contents)
}

type searchResult struct {
	PlayerID  string `json:"playerId"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
}
