package cache

import (
	"net/http"
	"strings"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

// notModifiedHeaders are the representation metadata a 304 should carry so a
// client or intermediary can refresh its stored response (RFC 9110 §15.4.5).
var notModifiedHeaders = []string{
	"ETag",
	"Vary",
	"Cache-Control",
	"Expires",
	"Last-Modified",
	"Content-Location",
}

// replay writes a cached entry to the response.
//
// Stored headers are sanitized again on the way out: hop-by-hop headers and
// cookies never reach a different client, and Vary always advertises every
// header the key was built from.
func replay(c *echo.Context, entry *store.Entry, config Config) error {
	// Cast map to http.Header for the .Get() helpers.
	headers := sanitizeHeader(http.Header(entry.Header))
	ensureVary(headers, config.VaryHeaders)

	serverETag := headers.Get("ETag")
	requestETags := strings.Join(c.Request().Header.Values("If-None-Match"), ",")

	if etagMatches(requestETags, serverETag) {
		for _, name := range notModifiedHeaders {
			for _, value := range headers.Values(name) {
				c.Response().Header().Add(name, value)
			}
		}
		c.Response().Header().Set("X-Cache", "HIT")
		return c.NoContent(http.StatusNotModified)
	}

	// Transfer headers to the actual response.
	for name, values := range headers {
		for _, value := range values {
			c.Response().Header().Add(name, value)
		}
	}
	c.Response().Header().Set("X-Cache", "HIT")

	// Ensure Content-Type is provided or use the cached one.
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return c.Blob(entry.Status, contentType, entry.Body)
}
