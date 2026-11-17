package abuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyRecorder remembers the keys written to the counter.
type keyRecorder struct {
	store.Counter

	mu   sync.Mutex
	keys []string
}

func (k *keyRecorder) Increment(ctx context.Context, key string, n int, ttl time.Duration) (int, error) {
	k.mu.Lock()
	k.keys = append(k.keys, key)
	k.mu.Unlock()
	return k.Counter.Increment(ctx, key, n, ttl)
}

func (k *keyRecorder) recordedKeys() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

func doRequest(e *echo.Echo, handler echo.HandlerFunc, req *http.Request) (*httptest.ResponseRecorder, error) {
	rec := httptest.NewRecorder()
	return rec, handler(e.NewContext(req, rec))
}

func TestSkipperIsHonored(t *testing.T) {
	e := echo.New()
	counter := &keyRecorder{Counter: store.NewMemoryCounter()}

	handler := New(Config{
		Store:   counter,
		Skipper: func(c *echo.Context) bool { return c.Request().URL.Path == "/healthz" },
		Rules:   []Rule{{Path: "/evil", Score: 100}},
	})(okHandler)

	for i := 0; i < 5; i++ {
		rec, err := doRequest(e, handler, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Empty(t, counter.recordedKeys(), "a skipped request must not touch the counter")

	_, err := doRequest(e, handler, httptest.NewRequest(http.MethodGet, "/evil", nil))
	require.Error(t, err, "the same middleware must still score non-skipped paths")
	assert.Len(t, counter.recordedKeys(), 1)
}

func TestCustomKeyFuncCannotBeSpoofedByXFF(t *testing.T) {
	e := echo.New()
	counter := &keyRecorder{Counter: store.NewMemoryCounter()}

	handler := New(Config{
		Store:     counter,
		Threshold: 100,
		KeyFunc:   func(*echo.Context) string { return "tenant-a" },
		Rules:     []Rule{{Path: "/evil", Score: 50}},
	})(okHandler)

	// Each request claims a different client IP; the custom KeyFunc ignores it.
	for _, spoof := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		req := httptest.NewRequest(http.MethodGet, "/evil", nil)
		req.RemoteAddr = "203.0.113.9:1234"
		req.Header.Set(echo.HeaderXForwardedFor, spoof)
		_, _ = doRequest(e, handler, req)
	}

	// 50 + 50 -> blocked on the second hit, because all three share one key.
	assert.Equal(t, []string{"abuse:heat:tenant-a", "abuse:heat:tenant-a"}, counter.recordedKeys())
}

func TestForwardedIPKeyFuncIgnoresUntrustedPeerHeaders(t *testing.T) {
	keyFunc := NewForwardedIPKeyFunc()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set(echo.HeaderXForwardedFor, "1.2.3.4")

	e := echo.New()
	assert.Equal(t, "203.0.113.9", keyFunc(e.NewContext(req, nil)),
		"X-Forwarded-For must be ignored when the peer is not a trusted proxy")
}

func TestForwardedIPKeyFuncTrustsConfiguredProxy(t *testing.T) {
	keyFunc := NewForwardedIPKeyFunc()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set(echo.HeaderXForwardedFor, "198.51.100.7")

	e := echo.New()
	assert.Equal(t, "198.51.100.7", keyFunc(e.NewContext(req, nil)),
		"a trusted proxy's X-Forwarded-For must be used")
}

func TestNewIPKeyFunc(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set(echo.HeaderXForwardedFor, "1.2.3.4")

	e := echo.New()
	c := e.NewContext(req, nil)

	assert.Equal(t, "203.0.113.9", NewIPKeyFunc(echo.ExtractIPDirect())(c))
	assert.Equal(t, "203.0.113.9", NewIPKeyFunc(nil)(c))
}

func TestEmptyKeyFuncFallsBackToRealIP(t *testing.T) {
	e := echo.New()
	counter := &keyRecorder{Counter: store.NewMemoryCounter()}

	handler := New(Config{
		Store:   counter,
		KeyFunc: func(*echo.Context) string { return "" },
		Rules:   []Rule{{Path: "/evil", Score: 1}},
	})(okHandler)

	req := httptest.NewRequest(http.MethodGet, "/evil", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	_, err := doRequest(e, handler, req)
	require.NoError(t, err)

	require.Len(t, counter.recordedKeys(), 1)
	assert.Equal(t, "abuse:heat:203.0.113.9", counter.recordedKeys()[0])
}
