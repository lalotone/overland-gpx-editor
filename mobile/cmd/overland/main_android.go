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
				fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="1"><body style="background:#f6f4ec;color:#24372e;font:20px sans-serif;padding:64px 28px"><h1>Overland</h1><p>Preparing your maps…</p>`)
				if startupError != "" {
					fmt.Fprint(w, "<p>Could not start. Close and reopen Overland.</p>")
				}
				return
			}
			encoded, _ := json.Marshal(startURL)
			fmt.Fprintf(w, `<!doctype html><meta name="referrer" content="no-referrer"><script>location.replace(%s)</script>`, encoded)
		})},
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "Overland"})
	go func() {
		data := application.Android.StoragePath()
		if data == "" {
			log.Print("Android storage is unavailable")
			return
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
