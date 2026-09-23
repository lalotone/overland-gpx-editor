package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackgroundServiceTracksBackendJobs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("overland")
		if !assert.NoError(t, err) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		assert.Equal(t, "capability", cookie.Value)
		if r.URL.Path == "/offline/routing" {
			reply(w, map[string]any{"job": map[string]string{"state": "running"}})
		} else {
			reply(w, []any{})
		}
	}))
	defer upstream.Close()
	events := make(chan bool, 2)
	h := &Host{URL: upstream.URL, token: "capability", native: Native{Background: func(active bool) { events <- active }}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { h.monitorDownloads(ctx); close(done) }()
	select {
	case active := <-events:
		assert.True(t, active)
	case <-time.After(time.Second):
		t.Fatal("foreground service was not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background watcher did not stop")
	}
	require.False(t, <-events, "shutdown must release the foreground service")
}
