package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCacheContext(t *testing.T, e *echo.Echo, method, path string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestCacheMissThenHit(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "hello")
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/hello")
	require.NoError(t, handler(c1))
	assert.Equal(t, "hello", rec1.Body.String())
	assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))
	assert.NotEmpty(t, rec1.Header().Get("ETag"))
	assert.Equal(t, 1, calls)

	c2, rec2 := newCacheContext(t, e, http.MethodGet, "/hello")
	require.NoError(t, handler(c2))
	assert.Equal(t, "hello", rec2.Body.String())
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "HIT", rec2.Header().Get("X-Cache"))
	assert.Equal(t, rec1.Header().Get("ETag"), rec2.Header().Get("ETag"))
	assert.Equal(t, 1, calls, "a hit must not run the handler")
}

func TestCacheExactMaxBodySizeIsCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, MaxBodySize: 5})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "12345")
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/exact")
	require.NoError(t, handler(c1))
	assert.Equal(t, "12345", rec1.Body.String())
	assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))

	c2, rec2 := newCacheContext(t, e, http.MethodGet, "/exact")
	require.NoError(t, handler(c2))
	assert.Equal(t, "12345", rec2.Body.String())
	assert.Equal(t, "HIT", rec2.Header().Get("X-Cache"))
	assert.Equal(t, 1, calls)
}

func TestCacheOversizedResponseIsStreamedAndNotCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, MaxBodySize: 4})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "12345")
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/large")
	require.NoError(t, handler(c1))
	assert.Equal(t, "12345", rec1.Body.String())
	assert.Equal(t, "BYPASS", rec1.Header().Get("X-Cache"))

	c2, rec2 := newCacheContext(t, e, http.MethodGet, "/large")
	require.NoError(t, handler(c2))
	assert.Equal(t, "12345", rec2.Body.String())
	assert.Equal(t, 2, calls, "oversized responses must not be cached")

	_, err := st.Get(context.Background(), DefaultKeyGenerator(c1))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCacheStreamingResponseIsNotCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st})(func(c *echo.Context) error {
		calls++
		c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
		_, _ = c.Response().Write([]byte("one"))
		require.NoError(t, http.NewResponseController(c.Response()).Flush())
		_, _ = c.Response().Write([]byte("two"))
		return nil
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/stream")
	require.NoError(t, handler(c1))
	assert.Equal(t, "onetwo", rec1.Body.String())
	assert.True(t, rec1.Flushed)
	assert.Equal(t, "BYPASS", rec1.Header().Get("X-Cache"))

	c2, _ := newCacheContext(t, e, http.MethodGet, "/stream")
	require.NoError(t, handler(c2))
	assert.Equal(t, 2, calls, "streamed responses must not be cached")
}

func TestCacheNon200IsNotCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st})(func(c *echo.Context) error {
		calls++
		return c.NoContent(http.StatusNoContent)
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/empty")
	require.NoError(t, handler(c1))
	assert.Equal(t, http.StatusNoContent, rec1.Code)
	assert.Equal(t, "BYPASS", rec1.Header().Get("X-Cache"))

	c2, _ := newCacheContext(t, e, http.MethodGet, "/empty")
	require.NoError(t, handler(c2))
	assert.Equal(t, 2, calls)
}

func TestCacheHandlerErrorIsNotCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st})(func(c *echo.Context) error {
		calls++
		_ = c.String(http.StatusOK, "partial")
		return echo.NewHTTPError(http.StatusInternalServerError, "boom")
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/boom")
	err := handler(c1)
	require.Error(t, err)
	assert.Empty(t, rec1.Body.String(), "the partial body must be discarded")

	c2, _ := newCacheContext(t, e, http.MethodGet, "/boom")
	require.Error(t, handler(c2))
	assert.Equal(t, 2, calls)

	_, err = st.Get(context.Background(), DefaultKeyGenerator(c1))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCacheRestoresOriginalWriter(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()

	handler := New(Config{Store: st})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	c, _ := newCacheContext(t, e, http.MethodGet, "/restore")
	original := c.Response()

	require.NoError(t, handler(c))
	assert.Same(t, original, c.Response())
}

func TestCacheNonGETIsNotCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 2; i++ {
		c, rec := newCacheContext(t, e, http.MethodPost, "/post")
		require.NoError(t, handler(c))
		assert.Equal(t, "ok", rec.Body.String())
	}
	assert.Equal(t, 2, calls)
}

func TestCacheSkipper(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{
		Store:   st,
		Skipper: func(*echo.Context) bool { return true },
	})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 2; i++ {
		c, rec := newCacheContext(t, e, http.MethodGet, "/skip")
		require.NoError(t, handler(c))
		assert.Equal(t, "ok", rec.Body.String())
	}
	assert.Equal(t, 2, calls)
}

func TestCacheRestoresOriginalWriterOnError(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()

	handler := New(Config{Store: st})(func(c *echo.Context) error {
		_ = c.String(http.StatusOK, "partial")
		return echo.NewHTTPError(http.StatusInternalServerError, "boom")
	})

	c, _ := newCacheContext(t, e, http.MethodGet, "/error-restore")
	original := c.Response()

	err := handler(c)
	require.Error(t, err)
	assert.Same(t, original, c.Response(), "the recorder must not leak into Echo's error handling")
}

func TestCacheRestoresOriginalWriterOnPanic(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()

	handler := New(Config{Store: st})(func(*echo.Context) error {
		panic("handler boom")
	})

	c, _ := newCacheContext(t, e, http.MethodGet, "/panic-restore")
	original := c.Response()

	assert.Panics(t, func() { _ = handler(c) })
	assert.Same(t, original, c.Response())
}
