package transport

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// guard refuses requests a web page could make a browser send to this
// listener: a Host that does not name it (DNS rebinding) and an Origin from
// another site (CSRF). It runs before authentication and covers every route.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (s.LoopbackBind || len(s.AllowedHosts) > 0) && !s.hostAllowed(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]*domain.Error{
				"error": domain.E(domain.CodeAuthDenied, "Host header is not a loopback address or an allowed host"),
			})
			return
		}
		for _, origin := range r.Header.Values("Origin") {
			if !sameOrigin(origin, r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]*domain.Error{
					"error": domain.E(domain.CodeAuthDenied, "cross-origin request refused"),
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(hostport string) bool {
	full := strings.ToLower(strings.TrimSpace(hostport))
	name := hostName(full)
	if name == "" {
		return false
	}
	if name == "localhost" {
		return true
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, allowed := range s.AllowedHosts {
		// An entry without a port matches the host on any port.
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed != "" && (allowed == full || allowed == name) {
			return true
		}
	}
	return false
}

// hostName returns the host of a lower-case Host header value without its
// port, IPv6 brackets, or trailing dot.
func hostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return strings.TrimSuffix(host, ".")
}

// sameOrigin reports whether an Origin header names the host and port the
// request was addressed to. The scheme is not compared because a TLS proxy
// may terminate HTTPS in front of this listener.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// requireJSONBody rejects a write request whose body is not declared as JSON.
// Browsers can send other body types cross-origin without a preflight.
func requireJSONBody(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}
