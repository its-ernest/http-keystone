package idempotency

import (
	"net/http"
	"strings"
)

// hopByHopHeaders are connection-specific and must not be stored or replayed
// from a cache.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// sanitizeHeader returns a copy of h without hop-by-hop headers, cookies, and
// response metadata that must be regenerated on replay.
func sanitizeHeader(h http.Header) http.Header {
	out := h.Clone()

	for _, connection := range out.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		out.Del(name)
	}
	out.Del("Set-Cookie")
	out.Del("Date")
	out.Del("Age")

	return out
}

// replaceHeader overwrites dst with the contents of src.
func replaceHeader(dst http.Header, src http.Header) {
	for name := range dst {
		delete(dst, name)
	}
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
}
