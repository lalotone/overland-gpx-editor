// Package mcp implements the optional MCP server and its browser bridge.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxBridgeBodyBytes = 4 << 20
	maxQueuedCommands  = 32
	maxBrowserViews    = 8
	// The stream is deliberately exempt from the shared request throttle, so
	// it needs its own bound. Two per permitted view leaves room for a
	// reconnect overlapping the connection it replaces.
	maxCommandStreams = 2 * maxBrowserViews
	viewStaleAfter    = 5 * time.Second
	// Comment frames keep intermediaries and idle-connection reapers from
	// dropping a stream that is simply waiting for the next agent command.
	streamKeepAlive = 15 * time.Second
	// Scoped to the bridge so the capability is never attached to ordinary
	// API calls, and HttpOnly so page scripts cannot read it.
	sessionCookieName = "overland_mcp_bridge"
	sessionCookiePath = "/mcp/browser"
)

var (
	ErrNoActiveView = errors.New("no active Overland browser view; open the web app and keep the tab visible")
	ErrViewBusy     = errors.New("the active Overland browser view has too many pending commands")
)

type browserCommand struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type commandResult struct {
	Value json.RawMessage
	Error string
}

type pendingCommand struct {
	viewID string
	result chan commandResult
}

type browserView struct {
	snapshot json.RawMessage
	updated  time.Time
	active   bool
	sequence uint64
	commands []browserCommand
	// Buffered to one so queueing never blocks on a view whose event stream
	// is closed or busy; the stream re-reads the queue head when it wakes.
	notify chan struct{}
}

func newBrowserView() *browserView {
	return &browserView{notify: make(chan struct{}, 1)}
}

func wake(notify chan struct{}) {
	select {
	case notify <- struct{}{}:
	default:
	}
}

// Bridge brokers commands between MCP clients and the currently active web UI.
// It also serves the latest UI snapshot as MCP context.
type Bridge struct {
	token  string
	ctx    context.Context
	cancel context.CancelFunc
	now    func() time.Time
	nextID atomic.Uint64
	// Open command streams, bounded separately from the HTTP throttle.
	streams atomic.Int64

	mu      sync.Mutex
	views   map[string]*browserView
	pending map[string]*pendingCommand
}

// NewBridge creates a browser broker with a random per-process capability.
func NewBridge() (*Bridge, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Bridge{
		token:   hex.EncodeToString(raw),
		ctx:     ctx,
		cancel:  cancel,
		now:     time.Now,
		views:   make(map[string]*browserView),
		pending: make(map[string]*pendingCommand),
	}, nil
}

// Close cancels pending calls and disables the bridge.
func (b *Bridge) Close() {
	b.cancel()
	b.mu.Lock()
	clear(b.views)
	clear(b.pending)
	b.mu.Unlock()
}

// Call sends an allowlisted command to the active browser and waits for its
// result. Command validation belongs to the MCP tool boundary.
func (b *Bridge) Call(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	now := b.now()
	b.mu.Lock()
	viewID, view := b.activeViewLocked(now)
	if view == nil {
		b.mu.Unlock()
		return nil, ErrNoActiveView
	}
	if len(view.commands) >= maxQueuedCommands {
		b.mu.Unlock()
		return nil, ErrViewBusy
	}

	id := "command-" + formatUint(b.nextID.Add(1))
	pending := &pendingCommand{viewID: viewID, result: make(chan commandResult, 1)}
	b.pending[id] = pending
	view.commands = append(view.commands, browserCommand{ID: id, Name: name, Arguments: arguments})
	notify := view.notify
	b.mu.Unlock()
	wake(notify)

	select {
	case result := <-pending.result:
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		if len(result.Value) == 0 {
			return json.RawMessage(`{"ok":true}`), nil
		}
		return result.Value, nil
	case <-ctx.Done():
		b.removePending(id)
		return nil, ctx.Err()
	case <-b.ctx.Done():
		b.removePending(id)
		return nil, context.Canceled
	}
}

// ActiveSnapshot returns the freshest browser state, including the view ID an
// agent can use to identify the tab it is controlling.
func (b *Bridge) ActiveSnapshot() (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	viewID, view := b.activeViewLocked(b.now())
	if view == nil {
		return nil, ErrNoActiveView
	}
	return json.Marshal(struct {
		ViewID    string          `json:"viewId"`
		Active    bool            `json:"active"`
		UpdatedAt time.Time       `json:"updatedAt"`
		Sequence  uint64          `json:"sequence"`
		State     json.RawMessage `json:"state"`
	}{viewID, view.active, view.updated.UTC(), view.sequence, view.snapshot})
}

func (b *Bridge) activeViewLocked(now time.Time) (string, *browserView) {
	b.pruneViewsLocked(now)
	var selectedID string
	var selected *browserView
	for id, view := range b.views {
		if now.Sub(view.updated) > viewStaleAfter || len(view.snapshot) == 0 {
			continue
		}
		if selected == nil || (view.active && !selected.active) ||
			(view.active == selected.active && view.updated.After(selected.updated)) {
			selectedID, selected = id, view
		}
	}
	return selectedID, selected
}

func (b *Bridge) pruneViewsLocked(now time.Time) {
	for id, view := range b.views {
		if now.Sub(view.updated) <= viewStaleAfter || b.viewHasPendingLocked(id) {
			continue
		}
		delete(b.views, id)
	}
}

func (b *Bridge) viewHasPendingLocked(viewID string) bool {
	for _, pending := range b.pending {
		if pending.viewID == viewID {
			return true
		}
	}
	return false
}

func (b *Bridge) removePending(id string) {
	b.mu.Lock()
	pending := b.pending[id]
	delete(b.pending, id)
	var notify chan struct{}
	if pending != nil {
		if view := b.views[pending.viewID]; view != nil {
			notify = view.notify
			for i := range view.commands {
				if view.commands[i].ID == id {
					view.commands = append(view.commands[:i], view.commands[i+1:]...)
					break
				}
			}
		}
	}
	b.mu.Unlock()
	// An abandoned command must not block the one queued behind it.
	wake(notify)
}

func formatUint(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

// ServeHTTP exposes the private same-origin channel used by the React app.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if b.ctx.Err() != nil {
		writeBridgeError(w, http.StatusServiceUnavailable, "MCP browser bridge is stopping")
		return
	}
	if !loopbackBrowserRequest(r) {
		writeBridgeError(w, http.StatusForbidden, "MCP browser control is loopback-only")
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/session":
		b.handleSession(w)
	case !b.authorized(r):
		writeBridgeError(w, http.StatusUnauthorized, "invalid MCP browser capability")
	case r.Method == http.MethodPut && r.URL.Path == "/view":
		b.handleView(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		b.handleEvents(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/result":
		b.handleResult(w, r)
	default:
		writeBridgeError(w, http.StatusNotFound, "not found")
	}
}

// handleSession hands the page a path-scoped capability cookie. The value is
// never exposed to scripts, so EventSource can authenticate a stream that
// cannot carry an Authorization header.
func (b *Bridge) handleSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    b.token,
		Path:     sessionCookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Deliberately not Secure: the bridge is loopback-only plain HTTP,
		// and a Secure cookie would simply be dropped.
	})
	writeBridgeJSON(w, http.StatusOK, map[string]bool{"enabled": true})
}

func (b *Bridge) authorized(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return len(cookie.Value) == len(b.token) &&
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(b.token)) == 1
}

func (b *Bridge) handleView(w http.ResponseWriter, r *http.Request) {
	var update struct {
		ViewID   string          `json:"viewId"`
		Active   bool            `json:"active"`
		Sequence uint64          `json:"sequence"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := decodeBridgeJSON(w, r, &update); err != nil {
		writeBridgeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validViewID(update.ViewID) || update.Sequence == 0 || len(update.Snapshot) == 0 || update.Snapshot[0] != '{' || !json.Valid(update.Snapshot) {
		writeBridgeError(w, http.StatusBadRequest, "invalid browser view")
		return
	}

	b.mu.Lock()
	now := b.now()
	b.pruneViewsLocked(now)
	view := b.views[update.ViewID]
	if view == nil {
		if len(b.views) >= maxBrowserViews {
			b.mu.Unlock()
			writeBridgeError(w, http.StatusTooManyRequests, "too many active browser views")
			return
		}
		view = newBrowserView()
		b.views[update.ViewID] = view
	} else if update.Sequence <= view.sequence {
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	view.snapshot = append(view.snapshot[:0], update.Snapshot...)
	view.updated = now
	view.active = update.Active
	view.sequence = update.Sequence
	b.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents streams queued commands to one browser view. The page holds
// this open instead of polling, so an idle tab costs one connection rather
// than several requests per second.
func (b *Bridge) handleEvents(w http.ResponseWriter, r *http.Request) {
	viewID := r.URL.Query().Get("view_id")
	if !validViewID(viewID) {
		writeBridgeError(w, http.StatusBadRequest, "invalid view_id")
		return
	}
	b.mu.Lock()
	view := b.views[viewID]
	b.mu.Unlock()
	if view == nil {
		writeBridgeError(w, http.StatusNotFound, "unknown browser view; publish a view before streaming")
		return
	}
	if b.streams.Add(1) > maxCommandStreams {
		b.streams.Add(-1)
		writeBridgeError(w, http.StatusTooManyRequests, "too many open command streams")
		return
	}
	defer b.streams.Add(-1)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := http.NewResponseController(w)
	// A stream must outlive any write deadline inherited from the server.
	_ = stream.SetWriteDeadline(time.Time{})
	if err := stream.Flush(); err != nil {
		return
	}

	keepAlive := time.NewTicker(streamKeepAlive)
	defer keepAlive.Stop()
	delivered := ""
	for {
		command, queued, live := b.headCommand(viewID)
		if !live {
			// The view was pruned; let the page re-announce and reconnect.
			return
		}
		// Redelivery is what makes reconnecting safe: an unacknowledged
		// command is sent again and the page discards the duplicate.
		if queued && command.ID != delivered {
			payload, err := json.Marshal(command)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", command.ID, payload); err != nil {
				return
			}
			if err := stream.Flush(); err != nil {
				return
			}
			delivered = command.ID
		}

		select {
		case <-view.notify:
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := stream.Flush(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-b.ctx.Done():
			return
		}
	}
}

// headCommand reports the oldest unacknowledged command for a view, whether
// one is queued, and whether the view still exists.
func (b *Bridge) headCommand(viewID string) (browserCommand, bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	view := b.views[viewID]
	if view == nil {
		return browserCommand{}, false, false
	}
	if len(view.commands) == 0 {
		return browserCommand{}, false, true
	}
	return view.commands[0], true, true
}

func (b *Bridge) handleResult(w http.ResponseWriter, r *http.Request) {
	var result struct {
		ViewID string          `json:"viewId"`
		ID     string          `json:"id"`
		Value  json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := decodeBridgeJSON(w, r, &result); err != nil {
		writeBridgeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validViewID(result.ViewID) || len(result.ID) > 128 || (len(result.Value) > 0 && !json.Valid(result.Value)) {
		writeBridgeError(w, http.StatusBadRequest, "invalid command result")
		return
	}
	if len(result.Error) > 1000 {
		writeBridgeError(w, http.StatusBadRequest, "command error is too long")
		return
	}

	b.mu.Lock()
	pending := b.pending[result.ID]
	if pending == nil || pending.viewID != result.ViewID {
		b.mu.Unlock()
		writeBridgeError(w, http.StatusNotFound, "command is no longer pending")
		return
	}
	delete(b.pending, result.ID)
	var notify chan struct{}
	if view := b.views[pending.viewID]; view != nil {
		notify = view.notify
		for i := range view.commands {
			if view.commands[i].ID == result.ID {
				view.commands = append(view.commands[:i], view.commands[i+1:]...)
				break
			}
		}
	}
	b.mu.Unlock()
	// Let the stream advance to whatever was queued behind this command
	// instead of waiting for the next keep-alive tick.
	wake(notify)
	pending.result <- commandResult{Value: result.Value, Error: result.Error}
	w.WriteHeader(http.StatusNoContent)
}

func decodeBridgeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBridgeBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON body")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON body")
	}
	return nil
}

func validViewID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func loopbackBrowserRequest(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") || !loopbackHost(r.Host) {
		return false
	}
	if r.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return false
		}
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && loopbackHost(u.Host)
}

func loopbackHost(value string) bool {
	host := value
	if parsed, _, err := net.SplitHostPort(value); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func writeBridgeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeBridgeError(w http.ResponseWriter, status int, detail string) {
	writeBridgeJSON(w, status, map[string]string{"detail": detail})
}
