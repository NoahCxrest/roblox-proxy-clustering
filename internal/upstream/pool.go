package upstream

import (
	"net/url"
	"strings"
	"sync/atomic"
)

// Target represents a single upstream cluster endpoint.
type Target struct {
	base         *url.URL
	direct       bool
	directScheme string
}

// Resolve returns a fully-qualified URL assembled from the upstream base, path, and query string.
func (t *Target) Resolve(host, path, rawQuery string) *url.URL {
	if t.direct {
		return &url.URL{
			Scheme:   t.directScheme,
			Host:     host,
			Path:     ensureLeadingSlash(path),
			RawQuery: rawQuery,
		}
	}

	u := *t.base
	u.Path = joinURLPath(u.Path, path)
	u.RawQuery = rawQuery
	return &u
}

// Pool implements a lock-free round-robin selector over upstream targets.
type Pool struct {
	targets []*Target
	cursor  atomic.Uint64
}

// NewPool constructs a pool from the provided URLs.
func NewPool(urls []*url.URL) *Pool {
	targets := make([]*Target, len(urls))
	for i, u := range urls {
		target := &Target{}
		if isDirect(u.Scheme) {
			target.direct = true
			target.directScheme = directScheme(u)
		} else {
			clone := *u
			target.base = &clone
		}
		targets[i] = target
	}
	return &Pool{targets: targets}
}

// Next returns the next target in a round-robin fashion.
func (p *Pool) Next() *Target {
	idx := int(p.cursor.Add(1)-1) % len(p.targets)
	return p.targets[idx]
}

// Len reports how many upstream targets are available.
func (p *Pool) Len() int {
	return len(p.targets)
}

func joinURLPath(basePath, reqPath string) string {
	switch {
	case basePath == "":
		if reqPath == "" {
			return "/"
		}
		return ensureLeadingSlash(reqPath)
	case reqPath == "":
		return ensureLeadingSlash(basePath)
	default:
		b := ensureLeadingSlash(basePath)
		r := ensureLeadingSlash(reqPath)
		if b[len(b)-1] == '/' {
			return b + r[1:]
		}
		return b + r
	}
}

func ensureLeadingSlash(path string) string {
	if path == "" {
		return "/"
	}
	if path[0] != '/' {
		return "/" + path
	}
	return path
}

func isDirect(scheme string) bool {
	return strings.EqualFold(scheme, "direct") || strings.EqualFold(scheme, "origin")
}

func directScheme(u *url.URL) string {
	if v := strings.TrimSpace(u.Query().Get("scheme")); v != "" {
		return v
	}
	return "https"
}
