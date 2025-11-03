package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"roblox-proxy-clustering/internal/upstream"
)

const (
	userAgent            = "RobloxProxyCluster/1.0"
	hopHeaderConnection  = "Connection"
	hopHeaderKeepAlive   = "Keep-Alive"
	hopHeaderProxyAuth   = "Proxy-Authenticate"
	hopHeaderProxyAuthz  = "Proxy-Authorization"
	hopHeaderTe          = "TE"
	hopHeaderTrailer     = "Trailer"
	hopHeaderTransferEnc = "Transfer-Encoding"
	hopHeaderUpgrade     = "Upgrade"
)

var hopHeaders = map[string]struct{}{
	hopHeaderConnection:  {},
	hopHeaderKeepAlive:   {},
	hopHeaderProxyAuth:   {},
	hopHeaderProxyAuthz:  {},
	hopHeaderTe:          {},
	hopHeaderTrailer:     {},
	hopHeaderTransferEnc: {},
	hopHeaderUpgrade:     {},
}

// ForwardRequest models the data required to dispatch an upstream call through the cluster.
type ForwardRequest struct {
	Method        string
	RobloxHost    string
	Path          string
	UpstreamPath  string
	RawQuery      string
	Header        http.Header
	Body          io.Reader
	ContentLength int64
	OriginalHost  string
	RemoteAddr    string
}

// Client orchestrates low-latency HTTP calls across upstream clusters.
type Client struct {
	pool           *upstream.Pool
	httpClient     *http.Client
	requestTimeout time.Duration
}

// New constructs a tuned HTTP client bound to the upstream pool.
func New(pool *upstream.Pool, timeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 400 * time.Millisecond, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   400 * time.Millisecond,
		ExpectContinueTimeout: 150 * time.Millisecond,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       512,
	}

	return &Client{
		pool:           pool,
		httpClient:     &http.Client{Transport: transport},
		requestTimeout: timeout,
	}
}

// Forward issues an HTTP request to the selected upstream target.
func (c *Client) Forward(ctx context.Context, req *ForwardRequest) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("forward request cannot be nil")
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if req.RobloxHost == "" {
		return nil, errors.New("forward request missing RobloxHost")
	}

	target := c.pool.Next()
	resolved := target.Resolve(req.RobloxHost, req.Path, req.UpstreamPath, req.RawQuery)

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, resolved.String(), req.Body)
	if err != nil {
		return nil, err
	}

	httpReq.Host = req.RobloxHost
	cloneHeaders(httpReq.Header, req.Header)
	sanitizeHopHeaders(httpReq.Header)

	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Accept-Encoding", "identity")
	if req.OriginalHost != "" {
		httpReq.Header.Set("X-Forwarded-Host", req.OriginalHost)
	}

	if req.RemoteAddr != "" {
		forwardedFor := strings.TrimSpace(parseIP(req.RemoteAddr))
		if forwardedFor != "" {
			if prior := httpReq.Header.Get("X-Forwarded-For"); prior != "" {
				httpReq.Header.Set("X-Forwarded-For", prior+", "+forwardedFor)
			} else {
				httpReq.Header.Set("X-Forwarded-For", forwardedFor)
			}
		}
	}

	if req.ContentLength >= 0 {
		httpReq.ContentLength = req.ContentLength
	}

	return c.httpClient.Do(httpReq)
}

// FetchJSON performs a GET against the Roblox host and unmarshals the response into dst.

func (c *Client) FetchJSON(ctx context.Context, host, path string, query url.Values, dst any) error {
	headers := make(http.Header, 2)
	headers.Set("Accept", "application/json")
	resp, err := c.Forward(ctx, &ForwardRequest{
		Method:     http.MethodGet,
		RobloxHost: host,
		Path:       path,
		RawQuery:   query.Encode(),
		Header:     headers,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	if dst == nil {
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(dst)
}

// HTTPError encapsulates non-2xx upstream responses.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream error: status=%d body=%q", e.StatusCode, e.Body)
}

func cloneHeaders(dst, src http.Header) {
	if src == nil {
		return
	}
	for k, vv := range src {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func sanitizeHopHeaders(h http.Header) {
	for key := range h {
		canonical := textproto.CanonicalMIMEHeaderKey(key)
		if _, ok := hopHeaders[canonical]; ok {
			h.Del(key)
		}
	}
}

func parseIP(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
