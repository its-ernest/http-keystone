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

func requestHandler(t *testing.T, e *echo.Echo, handler echo.HandlerFunc, path string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	return rec, handler(e.NewContext(req, rec))
}

func okHandler(c *echo.Context) error {
	return c.String(http.StatusOK, "ok")
}

func TestNewDefaultsZeroThreshold(t *testing.T) {
	t.Run("empty config never blocks", func(t *testing.T) {
		e := echo.New()
		handler := New(Config{Store: store.NewMemoryCounter()})(okHandler)

		for i := 0; i < 20; i++ {
			rec, err := requestHandler(t, e, handler, "/safe")
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, rec.Code)
		}
	})

	t.Run("zero and negative threshold behave like the default", func(t *testing.T) {
		for _, threshold := range []int{0, -5} {
			e := echo.New()
			handler := New(Config{
				Store:     store.NewMemoryCounter(),
				Threshold: threshold,
				Rules:     []Rule{{Path: "/probe", Score: 50}},
			})(okHandler)

			// 0 + 50 < 100: the first hit passes.
			rec, err := requestHandler(t, e, handler, "/probe")
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, rec.Code, "threshold %d must default to 100", threshold)

			// 50 + 50 >= 100: the second hit is blocked.
			_, err = requestHandler(t, e, handler, "/probe")
			require.Error(t, err)
			he, ok := err.(*echo.HTTPError)
			require.True(t, ok)
			assert.Equal(t, http.StatusForbidden, he.Code)
		}
	})

	t.Run("explicit threshold still wins", func(t *testing.T) {
		e := echo.New()
		handler := New(Config{
			Store:     store.NewMemoryCounter(),
			Threshold: 10,
			Rules:     []Rule{{Path: "/probe", Score: 10}},
		})(okHandler)

		_, err := requestHandler(t, e, handler, "/probe")
		require.Error(t, err)
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok)
		assert.Equal(t, http.StatusForbidden, he.Code)
	})
}

func TestNewCopiesRules(t *testing.T) {
	e := echo.New()
	rules := []Rule{{Path: "/evil", Score: 100}}

	handler := New(Config{
		Store:     store.NewMemoryCounter(),
		Threshold: 100,
		Rules:     rules,
	})(okHandler)

	// Mutating the caller's slice must not change the middleware.
	rules[0].Path = "/safe"
	rules[0].Score = 0
	rules = append(rules, Rule{Path: "/other", Score: 100})

	_, err := requestHandler(t, e, handler, "/evil")
	require.Error(t, err, "the original /evil rule must still apply")
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusForbidden, he.Code)
}

// recordingCounter records the TTL passed to Increment.
type recordingCounter struct {
	store.Counter

	mu  sync.Mutex
	ttl time.Duration
}

func (c *recordingCounter) Increment(ctx context.Context, key string, n int, ttl time.Duration) (int, error) {
	c.mu.Lock()
	c.ttl = ttl
	c.mu.Unlock()
	return c.Counter.Increment(ctx, key, n, ttl)
}

func (c *recordingCounter) recordedTTL() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl
}

func TestNewDefaultsNonPositiveCooldown(t *testing.T) {
	for _, cooldown := range []time.Duration{0, -time.Minute} {
		counter := &recordingCounter{Counter: store.NewMemoryCounter()}
		e := echo.New()

		handler := New(Config{
			Store:    counter,
			Cooldown: cooldown,
			Rules:    []Rule{{Path: "/hit", Score: 1}},
		})(okHandler)

		_, err := requestHandler(t, e, handler, "/hit")
		require.NoError(t, err)
		assert.Equal(t, DefaultConfig.Cooldown, counter.recordedTTL(), "cooldown %s must default", cooldown)
	}
}
