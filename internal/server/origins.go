package server

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coder/websocket"
)

// ParseOrigins reads FLEETLING_ORIGINS: exact origins, comma separated,
// like https://fleetling.example.com,https://server.lan:8443. They are for
// a reverse proxy that rewrites the Host header, which otherwise makes
// every browser request look cross-origin.
func ParseOrigins(s string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("FLEETLING_ORIGINS: %q is not an origin like https://fleetling.example.com", part)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
}

// acceptWS upgrades a websocket. The library refuses an Origin that
// doesn't match the Host; an Origin listed in FLEETLING_ORIGINS is
// accepted as an exact match, scheme included.
func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	opts := &websocket.AcceptOptions{}
	if o := strings.ToLower(r.Header.Get("Origin")); o != "" && slices.Contains(s.cfg.Origins, o) {
		// Checked above, exactly. The library's own patterns match the
		// host only, so its check is turned off for this one request.
		opts.InsecureSkipVerify = true
	}
	return websocket.Accept(w, r, opts)
}
