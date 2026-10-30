//go:build android

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/lalotone/overland-gpx-editor/mobile/frontend"
	"github.com/lalotone/overland-gpx-editor/mobile/host"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func init() { application.RegisterAndroidMain(main) }

func main() {
	var urlMu sync.RWMutex
	startURL, startupError := "", ""
	app := application.New(application.Options{
		Name: "Overland", Description: "Routes beyond the road",
		Assets: application.AssetOptions{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			urlMu.RLock()
			defer urlMu.RUnlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if startURL == "" {
				status := "Opening maps and routing…"
				reload := `<script>setTimeout(()=>location.reload(),100)</script>`
				if startupError != "" {
					status, reload = "Could not start. Close and reopen Overland.", ""
				}
				fmt.Fprintf(w, bootstrapPage, reload, status)
				return
			}
			encoded, _ := json.Marshal(startURL)
			fmt.Fprintf(w, `<!doctype html><meta name="referrer" content="no-referrer"><body style="margin:0;background:#1f3329"><script>location.replace(%s)</script>`, encoded)
		})},
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "Overland"})
	go func() {
		data := application.Android.StoragePath()
		if data == "" {
			log.Print("Android storage is unavailable")
			return
		}
		if err := host.UsePrivateTempDir(filepath.Join(data, "tmp")); err != nil {
			log.Printf("mobile temp dir: %v", err)
		}
		var locationMu sync.Mutex
		h, err := host.Start(filepath.Join(data, "overland"), "127.0.0.1:0", frontend.Assets(), host.Native{
			Background: func(active bool) {
				if active {
					application.Android.StartForegroundService(`{"title":"Overland","text":"Preparing offline maps and routing"}`)
				} else {
					application.Android.StopForegroundService()
				}
			},
			Open: func() (host.Document, error) {
				path, err := app.Dialog.OpenFile().AddFilter("GPX tracks", "*.gpx").PromptForSingleSelection()
				if err != nil {
					return host.Document{}, err
				}
				return host.ReadPickedFile(path)
			},
			Share: func(document host.Document) error {
				dir := filepath.Join(data, "exports")
				if err := os.MkdirAll(dir, 0700); err != nil {
					return err
				}
				path := filepath.Join(dir, document.Filename)
				if err := os.WriteFile(path, []byte(document.Content), 0600); err != nil {
					return err
				}
				payload, _ := json.Marshal(map[string]string{"path": path, "mime": "application/gpx+xml"})
				application.Android.Share(string(payload))
				return nil
			},
			Locate: func(ctx context.Context) (host.Location, error) {
				if !locationMu.TryLock() {
					return host.Location{}, errors.New("location request is already running")
				}
				defer locationMu.Unlock()
				result := make(chan json.RawMessage, 1)
				off := app.Event.On("common:location", func(event *application.CustomEvent) {
					data, _ := json.Marshal(event.Data)
					select {
					case result <- data:
					default:
					}
				})
				defer off()
				application.Android.GetLocation()
				select {
				case <-ctx.Done():
					return host.Location{}, errors.New("location timed out; check location permission")
				case data := <-result:
					var position struct {
						Lat   float64 `json:"lat"`
						Lng   float64 `json:"lng"`
						Error string  `json:"error"`
					}
					if err := json.Unmarshal(data, &position); err != nil {
						return host.Location{}, err
					}
					if position.Error != "" {
						return host.Location{}, errors.New(position.Error)
					}
					return host.Location{Lat: position.Lat, Lon: position.Lng}, nil
				}
			},
		})
		urlMu.Lock()
		if err != nil {
			startupError = err.Error()
			log.Printf("mobile startup: %v", err)
		} else {
			startURL = h.StartURL
		}
		urlMu.Unlock()
		if err == nil {
			app.OnShutdown(func() { _ = h.Close() })
		}
	}()
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// bootstrapPage is shown while the Go host starts. It is the web splash in
// mobile/frontend/index.html without its animation, so the native splash,
// this page and the app's own splash read as one continuous screen.
const bootstrapPage = `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">%s
<style>html,body{margin:0;height:100%%;background:#1f3329}body{display:flex;flex-direction:column;align-items:center;justify-content:center;
background:radial-gradient(120%% 70%% at 50%% 38%%,#2c4638 0%%,#1f3329 58%%,#182920 100%%);color:#f3f1e8;font-family:system-ui,sans-serif}
.w{margin-top:26px;font-size:21px;font-weight:800;letter-spacing:.32em;padding-left:.32em}.t{margin-top:8px;font-size:13px;color:#b9cbbd}
.s{position:absolute;bottom:44px;left:0;right:0;text-align:center;font-size:12px;color:#9fb4a5}</style>
<svg width="112" height="112" viewBox="0 0 108 108" aria-hidden="true"><path fill="#8fb39c" opacity=".55" d="M48 80 L70 44 L84 62 L91 52 L104 80 Z"/>
<path fill="#f3f1e8" d="M6 80 L38 28 L54 54 L64 38 L92 80 Z"/><path fill="none" stroke="#e0844f" stroke-width="5" stroke-linecap="round" d="M20 100 C 34 96, 44 92, 40 84 S 46 70, 54 66 S 58 56, 54 54"/>
<circle fill="#e0844f" cx="54" cy="54" r="4.5"/></svg><div class="w">OVERLAND</div><div class="t">Routes beyond the road</div><div class="s">%s</div>`
