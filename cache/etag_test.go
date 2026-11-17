package cache

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// primeETagCache serves the response once and returns its ETag. The handler
// records Cache-Control and Vary so the 304 metadata can be asserted.
func primeETagCache(t *testing.T, cfg Config) (*echo.Context, *httptest.ResponseRecorder, string) {
	t.Helper()

	e := echo.New()
	handler := New(cfg)(func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "max-age=60")
		c.Response().Header().Set("Vary", "Accept-Encoding")
		return c.String(http.StatusOK, "hello")
	})

	c, rec := newCacheContext(t, e, http.MethodGet, "/etag")
	require.NoError(t, handler(c))
	require.Equal(t, http.StatusOK, rec.Code)

	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)
	return c, rec, etag
}

func newETagStore() *store.MemoryStore {
	return store.NewMemoryStore()
}

func TestCacheETagIsQuotedStrongValidator(t *testing.T) {
	_, rec, etag := primeETagCache(t, Config{Store: newETagStore(), TTL: time.Minute})

	assert.Regexp(t, `^"[0-9a-f]{64}"$`, etag)
	assert.True(t, strings.HasPrefix(rec.Header().Get("ETag"), `"`))
}

func TestCacheETagCanBeWeak(t *testing.T) {
	_, _, etag := primeETagCache(t, Config{
		Store:    newETagStore(),
		TTL:      time.Minute,
		WeakETag: true,
	})

	assert.Regexp(t, `^W/"[0-9a-f]{64}"$`, etag)
}

func TestCacheIfNoneMatch(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ifNoneMatch func(etag string) string
		wantStatus  int
	}{
		{"strong match", func(etag string) string { return etag }, http.StatusNotModified},
		{"weak client validator", func(etag string) string { return "W/" + etag }, http.StatusNotModified},
		{"list with match", func(etag string) string { return `"other", ` + etag }, http.StatusNotModified},
		{"whitespace around list", func(etag string) string { return ` "other" ,  ` + etag + ` ` }, http.StatusNotModified},
		{"star", func(string) string { return "*" }, http.StatusNotModified},
		{"no match", func(string) string { return `"other"` }, http.StatusOK},
		{"weak mismatch", func(etag string) string { return `W/"other"` }, http.StatusOK},
		{"empty", func(string) string { return "" }, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			st := newETagStore()
			calls := 0
			handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
				calls++
				c.Response().Header().Set("Cache-Control", "max-age=60")
				return c.String(http.StatusOK, "hello")
			})

			prime, primeRec := newCacheContext(t, e, http.MethodGet, "/conditional")
			require.NoError(t, handler(prime))
			etag := primeRec.Header().Get("ETag")
			require.NotEmpty(t, etag)

			req := httptest.NewRequest(http.MethodGet, "/conditional", nil)
			if value := tc.ifNoneMatch(etag); value != "" {
				req.Header.Set("If-None-Match", value)
			}
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			require.NoError(t, handler(c))
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, 1, calls, "a conditional hit must not run the handler again")

			if tc.wantStatus == http.StatusNotModified {
				assert.Empty(t, rec.Body.String(), "304 must not include a body")
				assert.Equal(t, etag, rec.Header().Get("ETag"))
				assert.Equal(t, "max-age=60", rec.Header().Get("Cache-Control"))
				assert.Contains(t, rec.Header().Values("Vary"), "Accept-Encoding")
				assert.Equal(t, "HIT", rec.Header().Get("X-Cache"))
			}
		})
	}
}

func TestCacheIfNoneMatchAcrossMultipleHeaders(t *testing.T) {
	e := echo.New()
	handler := New(Config{Store: newETagStore(), TTL: time.Minute})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "hello")
	})

	prime, primeRec := newCacheContext(t, e, http.MethodGet, "/multi")
	require.NoError(t, handler(prime))
	etag := primeRec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	req := httptest.NewRequest(http.MethodGet, "/multi", nil)
	req.Header.Add("If-None-Match", `"other"`)
	req.Header.Add("If-None-Match", etag)
	rec := httptest.NewRecorder()

	require.NoError(t, handler(e.NewContext(req, rec)))
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestCacheWeakStoredETagMatchesStrongClient(t *testing.T) {
	e := echo.New()
	handler := New(Config{Store: newETagStore(), TTL: time.Minute, WeakETag: true})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "hello")
	})

	prime, primeRec := newCacheContext(t, e, http.MethodGet, "/weak")
	require.NoError(t, handler(prime))
	weak := primeRec.Header().Get("ETag")
	require.True(t, strings.HasPrefix(weak, "W/"))

	req := httptest.NewRequest(http.MethodGet, "/weak", nil)
	req.Header.Set("If-None-Match", strings.TrimPrefix(weak, "W/"))
	rec := httptest.NewRecorder()

	require.NoError(t, handler(e.NewContext(req, rec)))
	assert.Equal(t, http.StatusNotModified, rec.Code, "weak comparison ignores the W/ prefix")
}

func TestSplitETags(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"single", `"abc"`, []string{`"abc"`}},
		{"list", `"abc", "def"`, []string{`"abc"`, `"def"`}},
		{"comma inside tag", `"a,b", W/"c"`, []string{`"a,b"`, `W/"c"`}},
		{"whitespace", `  "abc" ,   W/"def"  `, []string{`"abc"`, `W/"def"`}},
		{"star", `*`, []string{`*`}},
		{"empty", ``, nil},
		{"only commas", `, ,`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, splitETags(tc.in))
		})
	}
}

func TestETagMatches(t *testing.T) {
	const strong = `"abc"`
	const weak = `W/"abc"`

	assert.True(t, etagMatches(strong, strong))
	assert.True(t, etagMatches(weak, strong), "If-None-Match uses weak comparison")
	assert.True(t, etagMatches(strong, weak), "weak stored tags also match")
	assert.True(t, etagMatches(`"other", W/"abc"`, strong))
	assert.True(t, etagMatches("*", strong))
	assert.True(t, etagMatches(`*`, weak))

	assert.False(t, etagMatches("", strong))
	assert.False(t, etagMatches(`"other"`, strong))
	assert.False(t, etagMatches(`"ABC"`, strong), "opaque tags are case-sensitive")
	assert.False(t, etagMatches(strong, ""))
}
