package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBridgeStreamsCommandsOverSSE(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)

	client, base := bridgeTestServer(t, bridge)
	// The capability cookie is path-scoped, so the session request must be
	// what teaches the jar to send it to the stream.
	session, err := client.Get(base + "/mcp/browser/session")
	if err != nil {
		t.Fatal(err)
	}
	session.Body.Close()
	if session.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d", session.StatusCode)
	}
	publishView(t, client, base, "view-one", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, reader := openStream(t, ctx, client, base, "view-one")
	defer stream.Close()

	resultCh := make(chan json.RawMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		value, callErr := bridge.Call(ctx, "plan_route", json.RawMessage(`{"points":[]}`))
		resultCh <- value
		errCh <- callErr
	}()

	command := readCommandFrame(t, reader)
	if command.Name != "plan_route" || command.ID == "" {
		t.Fatalf("streamed command = %+v", command)
	}

	// Dropping the stream before acknowledging must redeliver, otherwise a
	// reconnect would silently lose the command.
	stream.Close()
	reconnect, reconnectReader := openStream(t, ctx, client, base, "view-one")
	defer reconnect.Close()
	if redelivered := readCommandFrame(t, reconnectReader); redelivered.ID != command.ID {
		t.Fatalf("redelivered command = %q, want %q", redelivered.ID, command.ID)
	}

	postResult(t, client, base, `{"viewId":"view-one","id":"`+command.ID+`","result":{"ok":true,"pointCount":2}}`)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if got := string(<-resultCh); got != `{"ok":true,"pointCount":2}` {
		t.Fatalf("result = %s", got)
	}
}

func TestBridgeStreamRequiresCapabilityAndKnownView(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/events?view_id=view-one", nil)
	request.Host = "127.0.0.1:8000"
	request.RemoteAddr = "127.0.0.1:32000"
	bridge.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("uncredentialed stream status = %d, want 401", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	bridge.ServeHTTP(recorder, authorizedRequest(bridge, http.MethodGet, "/events?view_id=never-published", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown view stream status = %d, want 404", recorder.Code)
	}
}

func TestBridgeSessionCookieIsScopedAndHTTPOnly(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/session", nil)
	request.Host = "127.0.0.1:8000"
	request.RemoteAddr = "127.0.0.1:32000"
	bridge.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("session status = %d", recorder.Code)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("session set %d cookies, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName || cookie.Value != bridge.token {
		t.Fatalf("cookie = %s=%s", cookie.Name, cookie.Value)
	}
	if !cookie.HttpOnly || cookie.Path != sessionCookiePath || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie is not scoped: %+v", cookie)
	}
	// The capability must not also leak through the response body.
	if strings.Contains(recorder.Body.String(), bridge.token) {
		t.Fatalf("session body leaked the capability: %s", recorder.Body.String())
	}
}

func TestBridgeBoundsOpenCommandStreams(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	client, base := bridgeTestServer(t, bridge)
	session, err := client.Get(base + "/mcp/browser/session")
	if err != nil {
		t.Fatal(err)
	}
	session.Body.Close()
	publishView(t, client, base, "view-one", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range maxCommandStreams {
		stream, _ := openStream(t, ctx, client, base, "view-one")
		t.Cleanup(func() { stream.Close() })
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/mcp/browser/events?view_id=view-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream beyond the cap status = %d, want 429", response.StatusCode)
	}
}

func bridgeTestServer(t *testing.T, bridge *Bridge) (*http.Client, string) {
	t.Helper()
	mux := http.NewServeMux()
	// Mirror the production mount so the cookie path is exercised for real.
	mux.Handle("/mcp/browser/", http.StripPrefix("/mcp/browser", bridge))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}, server.URL
}

func publishView(t *testing.T, client *http.Client, base, viewID string, sequence uint64) {
	t.Helper()
	body := `{"viewId":"` + viewID + `","sequence":` + formatUint(sequence) + `,"active":true,"snapshot":{"screen":"creation"}}`
	request, err := http.NewRequest(http.MethodPut, base+"/mcp/browser/view", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("view status = %d", response.StatusCode)
	}
}

func openStream(t *testing.T, ctx context.Context, client *http.Client, base, viewID string) (io.Closer, *bufio.Reader) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/mcp/browser/events?view_id="+viewID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		response.Body.Close()
		t.Fatalf("stream content type = %q", got)
	}
	return response.Body, bufio.NewReader(response.Body)
}

func readCommandFrame(t *testing.T, reader *bufio.Reader) browserCommand {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("event stream ended before a command arrived: %v", err)
		}
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
		if !ok {
			continue
		}
		var command browserCommand
		if err := json.Unmarshal([]byte(payload), &command); err != nil {
			t.Fatalf("invalid command frame %q: %v", payload, err)
		}
		return command
	}
}

func postResult(t *testing.T, client *http.Client, base, body string) {
	t.Helper()
	response, err := client.Post(base+"/mcp/browser/result", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("result status = %d", response.StatusCode)
	}
}

func TestBridgeSelectsFocusedViewAndExpiresStaleViews(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	bridge.now = func() time.Time { return now }
	putBrowserView(t, bridge, "background", false, `{"screen":"welcome"}`)
	putBrowserView(t, bridge, "focused", true, `{"screen":"view"}`)

	snapshot, err := bridge.ActiveSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(snapshot, []byte(`"viewId":"focused"`)) || !bytes.Contains(snapshot, []byte(`"screen":"view"`)) {
		t.Fatalf("snapshot = %s", snapshot)
	}

	now = now.Add(viewStaleAfter + time.Millisecond)
	if _, err := bridge.ActiveSnapshot(); err != ErrNoActiveView {
		t.Fatalf("stale snapshot error = %v, want %v", err, ErrNoActiveView)
	}
	if len(bridge.views) != 0 {
		t.Fatalf("stale views retained = %d", len(bridge.views))
	}
}

func TestBridgeBoundsActiveViews(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	for i := range maxBrowserViews {
		putBrowserView(t, bridge, "view-"+formatUint(uint64(i)), false, `{"screen":"welcome"}`)
	}
	body := []byte(`{"viewId":"one-too-many","sequence":1,"active":false,"snapshot":{"screen":"welcome"}}`)
	recorder := httptest.NewRecorder()
	bridge.ServeHTTP(recorder, authorizedRequest(bridge, http.MethodPut, "/view", body))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("view limit status = %d, want 429", recorder.Code)
	}
}

func TestBridgeSecurity(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)

	tests := []struct {
		name       string
		host       string
		remote     string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{"loopback", "127.0.0.1:8000", "127.0.0.1:32000", "http://127.0.0.1:8000", "same-origin", http.StatusOK},
		{"localhost dev origin", "127.0.0.1:8000", "127.0.0.1:32000", "http://localhost:5173", "same-site", http.StatusOK},
		{"host rebinding", "attacker.example", "127.0.0.1:32000", "", "same-origin", http.StatusForbidden},
		{"remote peer", "127.0.0.1:8000", "192.0.2.20:32000", "", "none", http.StatusForbidden},
		{"remote origin", "127.0.0.1:8000", "127.0.0.1:32000", "https://attacker.example", "same-site", http.StatusForbidden},
		{"cross-site", "127.0.0.1:8000", "127.0.0.1:32000", "", "cross-site", http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/session", nil)
			req.Host = test.host
			req.RemoteAddr = test.remote
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Sec-Fetch-Site", test.fetchSite)
			rec := httptest.NewRecorder()
			bridge.ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
			}
		})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events?view_id=view-one", nil)
	req.Host = "127.0.0.1:8000"
	req.RemoteAddr = "127.0.0.1:32000"
	bridge.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized command status = %d", rec.Code)
	}
}

func putBrowserView(t *testing.T, bridge *Bridge, id string, active bool, snapshot string) {
	t.Helper()
	bridge.mu.Lock()
	sequence := uint64(1)
	if current := bridge.views[id]; current != nil {
		sequence = current.sequence + 1
	}
	bridge.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"viewId": id, "sequence": sequence, "active": active, "snapshot": json.RawMessage(snapshot),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	bridge.ServeHTTP(rec, authorizedRequest(bridge, http.MethodPut, "/view", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("view status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func authorizedRequest(bridge *Bridge, method, target string, body []byte) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Host = "127.0.0.1:8000"
	req.RemoteAddr = "127.0.0.1:32000"
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: bridge.token})
	return req
}
