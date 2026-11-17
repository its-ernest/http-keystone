package internal

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseRecorderBuffersUntilCommit(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	rec.Header().Set("Content-Type", "text/plain")
	n, err := rec.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	assert.Empty(t, rr.Body.String(), "nothing should reach the client before Commit")
	assert.True(t, rec.Buffered())

	require.NoError(t, rec.Commit())
	assert.Equal(t, "hello", rr.Body.String())
	assert.Equal(t, "text/plain", rr.Header().Get("Content-Type"))
	assert.False(t, rec.Buffered())
}

func TestResponseRecorderExplicitStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	rec.WriteHeader(http.StatusCreated)
	_, err := rec.Write([]byte("created"))
	require.NoError(t, err)
	require.NoError(t, rec.Commit())

	assert.Equal(t, http.StatusCreated, rr.Code)
	assert.Equal(t, "created", rr.Body.String())
}

func TestResponseRecorderImplicit200(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	_, err := rec.Write([]byte("ok"))
	require.NoError(t, err)
	require.NoError(t, rec.Commit())

	assert.Equal(t, http.StatusOK, rec.Status())
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestResponseRecorderExactLimitIsBuffered(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 5)

	_, err := rec.Write([]byte("12345"))
	require.NoError(t, err)

	assert.True(t, rec.Buffered())
	assert.False(t, rec.Overflowed())
	assert.Empty(t, rr.Body.String())

	require.NoError(t, rec.Commit())
	assert.Equal(t, "12345", rr.Body.String())
}

func TestResponseRecorderOverflowStreams(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 4)
	rec.BeforeWriteHeader = func(h http.Header) {
		h.Set("X-Test", "before")
	}

	_, err := rec.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Empty(t, rr.Body.String())

	_, err = rec.Write([]byte("def"))
	require.NoError(t, err)

	assert.True(t, rec.Passthrough())
	assert.True(t, rec.Overflowed())
	assert.False(t, rec.Buffered())
	assert.Equal(t, "abcdef", rr.Body.String())
	assert.Equal(t, "before", rr.Header().Get("X-Test"))

	// Commit must not write the body a second time.
	require.NoError(t, rec.Commit())
	assert.Equal(t, "abcdef", rr.Body.String())
}

func TestResponseRecorderFlushDisablesBuffering(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	_, err := rec.Write([]byte("first"))
	require.NoError(t, err)

	rec.Flush()
	assert.True(t, rr.Flushed)
	assert.Equal(t, "first", rr.Body.String())
	assert.True(t, rec.Passthrough())

	_, err = rec.Write([]byte("second"))
	require.NoError(t, err)
	assert.Equal(t, "firstsecond", rr.Body.String())

	require.NoError(t, rec.Commit())
	assert.Equal(t, "firstsecond", rr.Body.String())
}

func TestResponseRecorderDiscard(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	_, err := rec.Write([]byte("secret"))
	require.NoError(t, err)

	rec.Discard()
	require.NoError(t, rec.Commit())

	assert.Empty(t, rr.Body.String())
	assert.False(t, rec.Buffered())
}

func TestResponseRecorderWriteAfterDiscardIsIgnored(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	rec.Discard()
	n, err := rec.Write([]byte("ignored"))
	require.NoError(t, err)
	assert.Equal(t, len("ignored"), n)

	require.NoError(t, rec.Commit())
	assert.Empty(t, rr.Body.String())
}

func TestResponseRecorderUnwrap(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	assert.Same(t, rr, rec.Unwrap())
}

func TestResponseRecorderBeforeWriteHeaderCalledOnce(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	calls := 0
	rec.BeforeWriteHeader = func(h http.Header) {
		calls++
		h.Set("X-Cache", "MISS")
	}

	rec.WriteHeader(http.StatusOK)
	rec.WriteHeader(http.StatusTeapot) // ignored
	_, err := rec.Write([]byte("body"))
	require.NoError(t, err)
	require.NoError(t, rec.Commit())

	assert.Equal(t, 1, calls)
	assert.Equal(t, http.StatusOK, rr.Code, "the first status wins")
	assert.Equal(t, "MISS", rr.Header().Get("X-Cache"))
}

func TestResponseRecorderLimitZeroStreamsImmediately(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 0)

	_, err := rec.Write([]byte("streamed"))
	require.NoError(t, err)

	assert.Equal(t, "streamed", rr.Body.String())
	assert.True(t, rec.Passthrough())
	assert.True(t, rec.Overflowed())
	assert.False(t, rec.Buffered())
}

func TestResponseRecorderHijackUnsupported(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	_, _, err := rec.Hijack()
	assert.Error(t, err)
	assert.True(t, rec.Hijacked())
}

// hijackableWriter is an http.ResponseWriter that also supports Hijack.
type hijackableWriter struct {
	http.ResponseWriter

	server    net.Conn
	client    net.Conn
	readWrite *bufio.ReadWriter
}

func (w *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.server, w.client = net.Pipe()
	w.readWrite = bufio.NewReadWriter(bufio.NewReader(w.client), bufio.NewWriter(w.client))
	return w.server, w.readWrite, nil
}

// pushWriter is an http.ResponseWriter that also supports HTTP/2 push.
type pushWriter struct {
	http.ResponseWriter
	targets []string
}

func (w *pushWriter) Push(target string, _ *http.PushOptions) error {
	w.targets = append(w.targets, target)
	return nil
}

func TestResponseRecorderBodyReturnsCopy(t *testing.T) {
	rec := NewResponseRecorder(httptest.NewRecorder(), 16)

	_, err := rec.Write([]byte("abc"))
	require.NoError(t, err)

	body := rec.Body()
	require.Equal(t, "abc", string(body))

	body[0] = 'X'
	assert.Equal(t, "abc", string(rec.Body()), "Body must not expose the internal buffer")
}

func TestResponseRecorderPush(t *testing.T) {
	pw := &pushWriter{ResponseWriter: httptest.NewRecorder()}
	rec := NewResponseRecorder(pw, 16)

	require.NoError(t, rec.Push("/style.css", nil))
	require.NoError(t, rec.Push("/app.js", nil))
	assert.Equal(t, []string{"/style.css", "/app.js"}, pw.targets)

	plain := NewResponseRecorder(httptest.NewRecorder(), 16)
	assert.ErrorIs(t, plain.Push("/style.css", nil), http.ErrNotSupported)
}

func TestResponseRecorderResponseControllerFlush(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 1024)

	_, err := rec.Write([]byte("chunk"))
	require.NoError(t, err)

	require.NoError(t, http.NewResponseController(rec).Flush())
	assert.True(t, rr.Flushed)
	assert.Equal(t, "chunk", rr.Body.String())
	assert.True(t, rec.Passthrough())
}

func TestResponseRecorderResponseControllerHijack(t *testing.T) {
	hw := &hijackableWriter{ResponseWriter: httptest.NewRecorder()}
	rec := NewResponseRecorder(hw, 1024)

	_, err := rec.Write([]byte("buffered"))
	require.NoError(t, err)

	conn, readWrite, err := http.NewResponseController(rec).Hijack()
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NotNil(t, readWrite)
	assert.True(t, rec.Hijacked())
	assert.False(t, rec.Buffered())
	t.Cleanup(func() {
		_ = conn.Close()
		_ = hw.client.Close()
	})

	// A hijacked response must not be written or committed afterwards.
	rec.WriteHeader(http.StatusTeapot)
	_, err = rec.Write([]byte("ignored"))
	assert.ErrorIs(t, err, errHijacked)
	require.NoError(t, rec.Commit())
}

func TestResponseRecorderFlushErrorAfterDiscard(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := NewResponseRecorder(rr, 16)

	rec.Discard()
	require.NoError(t, rec.FlushError())
	assert.False(t, rr.Flushed)
	assert.Empty(t, rr.Body.String())
}
