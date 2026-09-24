package host

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testIncomingID = "1790250000000-01234567-0123-4567-89ab-0123456789ab"

func incomingHost(t *testing.T, data string) *Host {
	t.Helper()
	// Native imports must remain usable even if routing/cache startup fails.
	h, err := start(data, "127.0.0.1:0", nil, Native{}, func(server.Config) (*server.Server, error) {
		return nil, errors.New("test backend unavailable")
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}

func incomingRequest(h *Host, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func stageIncoming(t *testing.T, data, id, name string) string {
	t.Helper()
	dir := filepath.Join(data, "incoming", id)
	require.NoError(t, os.MkdirAll(dir, 0700))
	metadata, err := json.Marshal(map[string]string{"filename": name})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), metadata, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "document.gpx"), []byte("<gpx/>"), 0600))
	return dir
}

func TestIncomingGPXSurvivesStartupUntilAcknowledged(t *testing.T) {
	data := t.TempDir()
	stageIncoming(t, data, testIncomingID, "Shared ride.gpx")
	stageIncoming(t, data, testIncomingID+".tmp", "Unfinished.gpx")
	h := incomingHost(t, data)
	list := incomingRequest(h, "GET", "/mobile/incoming")
	require.Equal(t, http.StatusOK, list.Code)
	assert.JSONEq(t, `[{"id":"`+testIncomingID+`","filename":"Shared ride.gpx"}]`, list.Body.String())
	path := "/mobile/incoming/" + testIncomingID
	for range 2 {
		read := incomingRequest(h, "GET", path)
		require.Equal(t, http.StatusOK, read.Code)
		assert.JSONEq(t, `{"filename":"Shared ride.gpx","content":"<gpx/>"}`, read.Body.String())
		assert.Equal(t, "no-store", read.Header().Get("Cache-Control"))
	}
	assert.NoFileExists(t, filepath.Join(data, "gpx", "Shared ride.gpx"), "receiving is not a library save")
	assert.Equal(t, http.StatusNoContent, incomingRequest(h, "DELETE", path).Code)
	assert.Equal(t, http.StatusNoContent, incomingRequest(h, "DELETE", path).Code)
	assert.JSONEq(t, `[]`, incomingRequest(h, "GET", "/mobile/incoming").Body.String())
	assert.NoDirExists(t, filepath.Join(data, "incoming", testIncomingID))
}

func TestIncomingGPXBoundary(t *testing.T) {
	data := t.TempDir()
	h := incomingHost(t, data)
	assert.JSONEq(t, `[]`, incomingRequest(h, "GET", "/mobile/incoming").Body.String())
	dir := stageIncoming(t, data, testIncomingID, "ride.gpx")
	path := "/mobile/incoming/" + testIncomingID
	for _, method := range []string{"GET", "DELETE"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
		r.Header.Set("Origin", "https://attacker.example")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code)
	}
	assert.Equal(t, http.StatusBadRequest, incomingRequest(h, "DELETE", "/mobile/incoming/../../draft.json").Code)
	assert.Equal(t, http.StatusMethodNotAllowed, incomingRequest(h, "POST", path).Code)
	assert.DirExists(t, dir)
	for _, name := range []string{"../secret.gpx", "ride.txt", ".hidden.gpx"} {
		stageIncoming(t, data, testIncomingID, name)
		assert.Equal(t, http.StatusBadRequest, incomingRequest(h, "GET", path).Code)
		assert.Contains(t, incomingRequest(h, "GET", "/mobile/incoming").Body.String(), "Unreadable shared GPX")
	}
	stageIncoming(t, data, testIncomingID, "ride.gpx")
	file, err := os.OpenFile(filepath.Join(dir, "document.gpx"), os.O_WRONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxDocument+1))
	require.NoError(t, file.Close())
	assert.Equal(t, http.StatusBadRequest, incomingRequest(h, "GET", path).Code)
	require.NoError(t, os.Remove(filepath.Join(dir, "document.gpx")))
	outside := filepath.Join(t.TempDir(), "secret.gpx")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "document.gpx")))
	result := incomingRequest(h, "GET", path)
	assert.Equal(t, http.StatusBadRequest, result.Code)
	assert.NotContains(t, result.Body.String(), "secret")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(strings.Repeat(" ", 1025)), 0600))
	assert.Equal(t, http.StatusBadRequest, incomingRequest(h, "GET", path).Code)
}
