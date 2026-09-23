package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapabilityAndDraft(t *testing.T) {
	h, err := Start(t.TempDir(), "127.0.0.1:0", nil, Native{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar}
	t.Cleanup(client.CloseIdleConnections)
	get := func(path string, want int) {
		t.Helper()
		response, err := client.Get(h.URL + path)
		require.NoError(t, err)
		defer response.Body.Close()
		assert.Equal(t, want, response.StatusCode)
	}
	get("/files", http.StatusUnauthorized)
	get("/mobile/start?token=wrong", http.StatusUnauthorized)
	get("/mobile/start?token="+h.token, http.StatusOK)
	get("/files", http.StatusOK)
	get("/mobile/draft", http.StatusOK)
	for _, tt := range []struct {
		origin, body string
		status       int
	}{
		{"https://attacker.example", `{"version":1}`, 403},
		{h.URL, `{"version":1}`, 200},
		{h.URL, `{bad`, 400},
	} {
		req, err := http.NewRequest("PUT", h.URL+"/mobile/draft", strings.NewReader(tt.body))
		require.NoError(t, err)
		req.Header.Set("Origin", tt.origin)
		response, err := client.Do(req)
		require.NoError(t, err)
		response.Body.Close()
		assert.Equal(t, tt.status, response.StatusCode)
	}
	response, err := client.Get(h.URL + "/mobile/draft")
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1}`, string(data), "malformed saves must preserve the previous draft")
	parsed, err := url.Parse(h.URL)
	require.NoError(t, err)
	assert.NotEmpty(t, jar.Cookies(parsed))
}

func TestNativeBoundary(t *testing.T) {
	shared := false
	h, err := Start(t.TempDir(), "127.0.0.1:0", nil, Native{
		Share:  func(Document) error { shared = true; return nil },
		Locate: func(context.Context) (Location, error) { return Location{Lat: 42, Lon: 1}, nil },
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	for _, filename := range []string{"../secret.gpx", "track.txt", "good.gpx"} {
		body, err := json.Marshal(Document{Filename: filename, Content: "<gpx/>"})
		require.NoError(t, err)
		req := httptest.NewRequest("POST", h.URL+"/mobile/share", strings.NewReader(string(body)))
		req.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if filename == "good.gpx" {
			assert.Equal(t, 200, response.Code)
			assert.True(t, shared)
		} else {
			assert.Equal(t, 400, response.Code)
			assert.False(t, shared)
		}
	}
}

func TestPickedFileValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ride.gpx")
	require.NoError(t, os.WriteFile(file, []byte("<gpx/>"), 0600))
	document, err := ReadPickedFile(file)
	require.NoError(t, err)
	assert.Equal(t, "ride.gpx", document.Filename)
	assert.Equal(t, "<gpx/>", document.Content)
	_, err = ReadPickedFile(filepath.Join(dir, "secret.txt"))
	assert.Error(t, err)
}
