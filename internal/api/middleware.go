package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/linxu/fmux/internal/ir"
)

type ctxKey int

const reqIDKey ctxKey = iota

// requestID attaches a stable identifier to every request and echoes it back,
// so one line of gateway log can be matched to one client-side complaint.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 128 {
			id = ir.NewID("req")
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqIDKey, id)))
	})
}

// RequestID returns the identifier attached by the middleware.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(reqIDKey).(string); ok {
		return v
	}
	return ""
}

// guard rejects requests that did not come from a local client.
//
// This is not paranoia. The gateway binds to loopback, but a web page open in
// any browser can POST to 127.0.0.1, and a `text/plain` body skips the CORS
// preflight entirely. Without a Host check, DNS rebinding turns any visited
// site into a client of the local model — and of the shell-adjacent services
// behind it. Rejecting foreign Host and Origin headers costs one map lookup.
func (s *Server) guard(next http.Handler) http.Handler {
	allowed := s.allowedHosts()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(r.Host)
		if host != "" && !allowed[host] {
			s.log.Warn("rejected foreign Host header", "host", r.Host, "req_id", RequestID(r.Context()))
			writeError(w, http.StatusForbidden, "host_not_allowed",
				"this gateway only serves "+strings.Join(sortedSet(allowed), ", ")+"; got Host "+r.Host)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			oh := originHost(origin)
			if oh == "" || !allowed[oh] {
				s.log.Warn("rejected cross-origin request", "origin", origin, "req_id", RequestID(r.Context()))
				writeError(w, http.StatusForbidden, "origin_not_allowed",
					"cross-origin requests are refused; this gateway is loopback-only")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHosts is the set of Host values the gateway answers to: the loopback
// names, whatever the listen address resolves to, and any explicit extras.
func (s *Server) allowedHosts() map[string]bool {
	out := map[string]bool{
		"127.0.0.1": true,
		"localhost": true,
		"::1":       true,
	}
	if s.cfg != nil {
		if h := normalizeHost(s.cfg.Listen); h != "" && h != "0.0.0.0" && h != "::" {
			out[h] = true
		}
		for _, h := range s.cfg.AllowedHosts {
			if h = normalizeHost(h); h != "" {
				out[h] = true
			}
		}
	}
	return out
}

func normalizeHost(h string) string {
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func originHost(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return normalizeHost(u.Host)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
