package host

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupServesUIAndDraftBeforeBackendRecovery(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	assets := fstest.MapFS{"index.html": {Data: []byte("mobile UI")}, "assets/app.js": {Data: []byte("app code")}}
	h, err := start(t.TempDir(), "127.0.0.1:0", assets, Native{}, func(config server.Config) (*server.Server, error) {
		<-release
		config.OpenFreeMapURL = ""
		return server.New(config)
	})
	require.NoError(t, err)
	t.Cleanup(func() { unblock(); require.NoError(t, h.Close()) })
	get := func(path string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if authenticated {
			r.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/", "/assets/app.js", "/mobile/draft", "/files"} {
		assert.Equal(t, http.StatusUnauthorized, get(path, false).Code)
	}
	assert.Contains(t, get("/", true).Body.String(), "mobile UI")
	assert.Contains(t, get("/assets/app.js", true).Body.String(), "app code")
	assert.Equal(t, http.StatusOK, get("/mobile/draft", true).Code)
	assert.Equal(t, http.StatusOK, get("/mobile/capabilities", true).Code)
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequest("GET", "/config", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled API request stayed blocked on initialization")
	}
	unblock()
	assert.Equal(t, http.StatusOK, get("/config", true).Code)
	assert.Equal(t, http.StatusOK, get("/files", true).Code)
}

func TestStartupFailureKeepsUIAvailable(t *testing.T) {
	h, err := start(t.TempDir(), "127.0.0.1:0", fstest.MapFS{"index.html": {Data: []byte("mobile UI")}}, Native{}, func(server.Config) (*server.Server, error) {
		return nil, errors.New("cache unavailable")
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	for _, path := range []string{"/config", "/", "/mobile/draft"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if path == "/config" {
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Contains(t, w.Body.String(), "cache unavailable")
		} else {
			assert.Equal(t, http.StatusOK, w.Code)
		}
	}
}
