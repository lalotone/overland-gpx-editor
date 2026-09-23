// Package host embeds the existing Overland backend in the mobile process.
package host

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lalotone/overland-gpx-editor/internal/server"
)

const maxDocument = 16 << 20

type Document struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Native struct {
	Open       func() (Document, error)
	Share      func(Document) error
	Locate     func(context.Context) (Location, error)
	Background func(bool)
}

type Host struct {
	URL      string
	StartURL string
	api      *server.Server
	http     *http.Server
	root     *os.Root
	native   Native
	token    string
	mu       sync.Mutex
	picker   sync.Mutex
	cancel   context.CancelFunc
	workers  sync.WaitGroup
}

// Start binds only loopback. The unguessable bootstrap capability becomes an
// HttpOnly session cookie, protecting the API from other local Android apps.
func Start(data, address string, assets fs.FS, native Native) (*Host, error) {
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(data)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		root.Close()
		return nil, err
	}
	if !listener.Addr().(*net.TCPAddr).IP.IsLoopback() {
		listener.Close()
		root.Close()
		return nil, errors.New("mobile host must listen on loopback")
	}
	h := &Host{URL: "http://" + listener.Addr().String(), root: root, native: native, token: rand.Text()}
	h.StartURL = h.URL + "/mobile/start?token=" + h.token
	h.api, err = server.New(server.Config{
		GPXDir: filepath.Join(data, "gpx"), Assets: assets,
		OfflineCacheDir: filepath.Join(data, "responses"), OfflineCacheMaxBytes: 4 << 30, OfflineCacheMaxEntries: 200000,
		ElevationTiles: true, ElevationTileCache: filepath.Join(data, "terrain"), ElevationTileCacheMaxBytes: 2 << 30,
		RoutingCacheDir: filepath.Join(data, "routing"), RoutingJobs: 1, RoutingConcurrency: 1,
		OpenFreeMapURL: "https://tiles.openfreemap.org/styles/liberty", OpenFreeMapAllowBulk: true,
	})
	if err != nil {
		listener.Close()
		root.Close()
		return nil, err
	}
	h.http = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	go func() { _ = h.http.Serve(listener) }()
	if native.Background != nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		h.workers.Go(func() { h.monitorDownloads(ctx) })
	}
	return h, nil
}

func (h *Host) Close() error {
	if h.cancel != nil {
		h.cancel()
		h.workers.Wait()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(h.http.Shutdown(ctx), h.api.Close(), h.root.Close())
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/mobile/start" && r.Method == http.MethodGet {
		if !h.matches(r.URL.Query().Get("token")) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "overland", Value: h.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.Header().Set("Cache-Control", "no-store")
		// Commit the loopback origin before navigating again: an HTTP redirect
		// from wails.localhost would withhold a SameSite=Strict cookie.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><meta name="referrer" content="no-referrer"><script>location.replace("/")</script>`)
		return
	}
	cookie, err := r.Cookie("overland")
	if err != nil || !h.matches(cookie.Value) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != h.URL {
		http.Error(w, "Origin not allowed", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/mobile/") {
		w.Header().Set("Cache-Control", "no-store")
		h.mobile(w, r)
		return
	}
	h.api.ServeHTTP(w, r)
}

func (h *Host) matches(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

func reply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (h *Host) mobile(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/mobile/capabilities" && r.Method == "GET":
		reply(w, map[string]bool{"native": h.native.Open != nil})
	case r.URL.Path == "/mobile/draft":
		h.draft(w, r)
	case r.URL.Path == "/mobile/import" && r.Method == "POST" && h.native.Open != nil:
		if !h.picker.TryLock() {
			http.Error(w, "A file picker is already open", http.StatusConflict)
			return
		}
		defer h.picker.Unlock()
		document, err := h.native.Open()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		reply(w, document)
	case r.URL.Path == "/mobile/share" && r.Method == "POST" && h.native.Share != nil:
		var document Document
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDocument)).Decode(&document); err != nil {
			http.Error(w, "Invalid document", 400)
			return
		}
		if err := server.ValidateGPXFilename(document.Filename); err != nil {
			http.Error(w, "Invalid GPX filename", 400)
			return
		}
		if err := h.native.Share(document); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		reply(w, map[string]bool{"ok": true})
	case r.URL.Path == "/mobile/location" && r.Method == "POST" && h.native.Locate != nil:
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		location, err := h.native.Locate(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		reply(w, location)
	default:
		http.NotFound(w, r)
	}
}

func (h *Host) draft(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Method {
	case "GET":
		data, err := h.root.ReadFile("draft.json")
		if errors.Is(err, os.ErrNotExist) {
			reply(w, nil)
			return
		}
		if err != nil {
			http.Error(w, "Cannot read draft", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	case "PUT":
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDocument))
		if err != nil {
			http.Error(w, "Draft is too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !json.Valid(data) {
			http.Error(w, "Invalid draft", 400)
			return
		}
		if err := h.root.WriteFile("draft.tmp", data, 0600); err != nil {
			http.Error(w, "Cannot save draft", 500)
			return
		}
		if err := h.root.Rename("draft.tmp", "draft.json"); err != nil {
			http.Error(w, "Cannot save draft", 500)
			return
		}
		reply(w, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ReadPickedFile bounds native picker copies before passing data to the WebView.
func ReadPickedFile(path string) (Document, error) {
	if path == "" {
		return Document{}, nil
	}
	if err := server.ValidateGPXFilename(filepath.Base(path)); err != nil {
		return Document{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDocument+1))
	if err != nil {
		return Document{}, err
	}
	if len(data) > maxDocument {
		return Document{}, fmt.Errorf("GPX exceeds %d MiB", maxDocument>>20)
	}
	return Document{Filename: filepath.Base(path), Content: string(data)}, nil
}
