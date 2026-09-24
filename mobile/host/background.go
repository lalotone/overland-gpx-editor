package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Monitor the real backend rather than WebView timers, which Android throttles
// when backgrounded. Only active acquisitions keep the foreground service up.
func (h *Host) monitorDownloads(ctx context.Context) {
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	active := false
	defer func() {
		if active {
			h.native.Background(false)
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var routing struct {
			Job *struct {
				State string `json:"state"`
			} `json:"job"`
		}
		var packs []struct {
			State string `json:"state"`
		}
		if h.readStatus(ctx, client, "/offline/routing", &routing) && h.readStatus(ctx, client, "/offline/packs", &packs) {
			next := routing.Job != nil && running(routing.Job.State)
			for _, pack := range packs {
				next = next || running(pack.State)
			}
			if next != active {
				h.native.Background(next)
				active = next
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func running(state string) bool { return state == "queued" || state == "running" }

func (h *Host) readStatus(ctx context.Context, client *http.Client, path string, result any) bool {
	request, err := http.NewRequestWithContext(ctx, "GET", h.URL+path, nil)
	if err != nil {
		return false
	}
	request.AddCookie(&http.Cookie{Name: "overland", Value: h.token})
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result) == nil
}
