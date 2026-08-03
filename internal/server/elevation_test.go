package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeDEM stands in for an upstream elevation service, recording what each
// request asked for. It answers in either provider's dialect.
type fakeDEM struct {
	mu        sync.Mutex
	requests  [][]string // locations ("lat,lon") per request
	datasets  []string
	shortBy   int  // return this many fewer results than requested
	failWith  int  // non-zero: respond with this status
	openMeteo bool // answer in Open-Meteo's shape
	*httptest.Server
}

func newFakeDEM(t *testing.T, openMeteo bool) *fakeDEM {
	t.Helper()
	f := &fakeDEM{openMeteo: openMeteo}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		var locations []string
		var dataset string
		if openMeteo {
			lats := strings.Split(q.Get("latitude"), ",")
			lons := strings.Split(q.Get("longitude"), ",")
			for i := range lats {
				if i < len(lons) {
					locations = append(locations, lats[i]+","+lons[i])
				}
			}
			dataset = openMeteoDataset
		} else {
			locations = strings.Split(q.Get("locations"), "|")
			dataset = strings.TrimPrefix(r.URL.Path, "/v1/")
		}

		f.mu.Lock()
		f.requests = append(f.requests, locations)
		f.datasets = append(f.datasets, dataset)
		short := f.shortBy
		fail := f.failWith
		f.mu.Unlock()

		if fail != 0 {
			http.Error(w, "upstream is down", fail)
			return
		}

		count := len(locations) - short
		if openMeteo {
			values := make([]string, 0, count)
			for i := 0; i < count; i++ {
				values = append(values, fmt.Sprintf("%d", 100+i))
			}
			fmt.Fprintf(w, `{"elevation":[%s]}`, strings.Join(values, ","))
			return
		}
		results := make([]string, 0, count)
		for i := 0; i < count; i++ {
			results = append(results,
				fmt.Sprintf(`{"elevation":%d,"location":%q}`, 100+i, locations[i]))
		}
		fmt.Fprintf(w, `{"results":[%s],"status":"OK"}`, strings.Join(results, ","))
	}))
	t.Cleanup(f.Close)
	return f
}

// newElevationServer wires a server to a self-hosted opentopodata upstream.
func newElevationServer(t *testing.T, dem *fakeDEM) *Server {
	t.Helper()
	s, err := New(Config{
		GPXDir:           t.TempDir(),
		ElevationHost:    dem.URL,
		ElevationDataset: "srtm30m",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newOpenMeteoServer wires a server with no ElevationHost, so it takes the
// Open-Meteo path, pointed at a stub.
func newOpenMeteoServer(t *testing.T, dem *fakeDEM) *Server {
	t.Helper()
	previous := openMeteoURL
	openMeteoURL = dem.URL
	t.Cleanup(func() { openMeteoURL = previous })

	s, err := New(Config{GPXDir: t.TempDir(), ElevationDataset: "srtm30m"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func postBatch(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/elevation/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) elevationResponse {
	t.Helper()
	var got elevationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	return got
}

func locationsPayload(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("42.%06d,-0.400000", i)
	}
	return strings.Join(parts, "|")
}

// The frontend sends up to 6000 points; upstream caps a request at 100, so the
// proxy must chunk and stitch rather than pass the whole set through.
func TestBatchChunksAtUpstreamLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		openMeteo bool
	}{{"opentopodata", false}, {"open-meteo", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dem := newFakeDEM(t, tc.openMeteo)
			s := newElevationServer(t, dem)
			if tc.openMeteo {
				s = newOpenMeteoServer(t, dem)
			}

			const total = 250
			rec := postBatch(t, s, fmt.Sprintf(`{"locations":%q}`, locationsPayload(total)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}

			got := decodeBatch(t, rec)
			if len(got.Results) != total {
				t.Fatalf("results = %d, want %d", len(got.Results), total)
			}
			if len(dem.requests) != 3 {
				t.Fatalf("upstream requests = %d, want 3", len(dem.requests))
			}
			for i, want := range []int{100, 100, 50} {
				if len(dem.requests[i]) != want {
					t.Errorf("request %d carried %d locations, want %d", i, len(dem.requests[i]), want)
				}
			}

			// Coordinates are normalised on the way through: they are parsed
			// and re-formatted, so trailing zeroes go. Same point, shorter URL.
			if got := dem.requests[0][0]; got != "42,-0.4" {
				t.Errorf("first location = %q, want %q", got, "42,-0.4")
			}
			// Order must survive the stitching: result i belongs to point i.
			if got := dem.requests[2][49]; got != "42.000249,-0.4" {
				t.Errorf("last location = %q, want %q", got, "42.000249,-0.4")
			}
			if got.Results[0].Elevation == nil || *got.Results[0].Elevation != 100 {
				t.Errorf("first elevation = %v", got.Results[0].Elevation)
			}
			if got.Results[100].Elevation == nil || *got.Results[100].Elevation != 100 {
				t.Errorf("first of the second chunk = %v", got.Results[100].Elevation)
			}
		})
	}
}

func TestBatchPadsShortUpstreamResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		openMeteo bool
	}{{"opentopodata", false}, {"open-meteo", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dem := newFakeDEM(t, tc.openMeteo)
			dem.shortBy = 3
			s := newElevationServer(t, dem)
			if tc.openMeteo {
				s = newOpenMeteoServer(t, dem)
			}

			rec := postBatch(t, s, fmt.Sprintf(`{"locations":%q}`, locationsPayload(10)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}

			got := decodeBatch(t, rec)
			if len(got.Results) != 10 {
				t.Fatalf("results = %d, want 10 — arrays must stay aligned with the request", len(got.Results))
			}
			// A missing sample must come back null, never 0: the profile would
			// read a zero as sea level and invent a cliff.
			for _, i := range []int{7, 8, 9} {
				if got.Results[i].Elevation != nil {
					t.Errorf("padded result %d = %v, want null", i, *got.Results[i].Elevation)
				}
			}
		})
	}
}

// With no ElevationHost configured the proxy must reach for Open-Meteo, so the
// binary is useful with no DEM service on the network.
func TestDefaultsToOpenMeteo(t *testing.T) {
	dem := newFakeDEM(t, true)
	s := newOpenMeteoServer(t, dem)

	rec := postBatch(t, s, `{"locations":"42.5,-0.4","dataset":"srtm30m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if len(dem.requests) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(dem.requests))
	}
	if got := dem.requests[0][0]; got != "42.5,-0.4" {
		t.Errorf("location = %q", got)
	}
	// The dataset is reported honestly: Open-Meteo serves Copernicus 90 m
	// whatever the client asked for.
	if got := decodeBatch(t, rec); got.Dataset != openMeteoDataset {
		t.Errorf("dataset = %q, want %q", got.Dataset, openMeteoDataset)
	}
}

func TestSelfHostedHostTakesPrecedence(t *testing.T) {
	dem := newFakeDEM(t, false)
	s := newElevationServer(t, dem)

	if rec := postBatch(t, s, `{"locations":"42.5,-0.4"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if dem.datasets[0] != "srtm30m" {
		t.Errorf("dataset = %q, want the configured srtm30m", dem.datasets[0])
	}
}

func TestBatchAcceptsArrayLocations(t *testing.T) {
	dem := newFakeDEM(t, false)
	s := newElevationServer(t, dem)

	rec := postBatch(t, s, `{"locations":[[42.5,-0.4],"41.6,-0.9"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	want := []string{"42.5,-0.4", "41.6,-0.9"}
	for i, w := range want {
		if dem.requests[0][i] != w {
			t.Errorf("location %d = %q, want %q", i, dem.requests[0][i], w)
		}
	}
}

func TestBatchRejectsBadLocations(t *testing.T) {
	dem := newFakeDEM(t, false)
	s := newElevationServer(t, dem)

	bad := []string{
		`{}`,
		`{"locations":""}`,
		`{"locations":"  |  "}`,
		`{"locations":7}`,
		`not json`,
		`{"locations":"42.5"}`,        // no comma
		`{"locations":"north,west"}`,  // not numbers
		`{"locations":"91.0,-0.4"}`,   // off the planet
		`{"locations":"42.5,-200.0"}`, // ditto
	}
	for _, body := range bad {
		if rec := postBatch(t, s, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if len(dem.requests) != 0 {
		t.Errorf("%d malformed requests reached the upstream service", len(dem.requests))
	}
}

// An unknown dataset must fall back to the configured default rather than be
// forwarded — it lands in the upstream URL path.
func TestUnknownDatasetFallsBackToDefault(t *testing.T) {
	dem := newFakeDEM(t, false)
	s := newElevationServer(t, dem)

	postBatch(t, s, `{"locations":"42.5,-0.4","dataset":"../../etc/passwd"}`)
	postBatch(t, s, `{"locations":"42.5,-0.4","dataset":"eudem25m"}`)

	if len(dem.datasets) != 2 {
		t.Fatalf("requests = %d, want 2", len(dem.datasets))
	}
	if dem.datasets[0] != "srtm30m" {
		t.Errorf("unknown dataset forwarded as %q, want srtm30m", dem.datasets[0])
	}
	if dem.datasets[1] != "eudem25m" {
		t.Errorf("allowed dataset = %q, want eudem25m", dem.datasets[1])
	}
}

func TestUpstreamFailureIsBadGateway(t *testing.T) {
	dem := newFakeDEM(t, false)
	dem.failWith = http.StatusInternalServerError
	s := newElevationServer(t, dem)

	if rec := postBatch(t, s, `{"locations":"42.5,-0.4"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("batch status = %d, want 502", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/elevation?lat=42.5&lon=-0.4", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("single status = %d, want 502", rec.Code)
	}
}

// Open-Meteo reports refusals in the body with a 200-shaped payload, so the
// reason has to be surfaced rather than decoded as "no elevation".
func TestOpenMeteoErrorBodyIsReported(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":true,"reason":"Hourly request limit exceeded"}`)
	}))
	t.Cleanup(stub.Close)

	previous := openMeteoURL
	openMeteoURL = stub.URL
	t.Cleanup(func() { openMeteoURL = previous })

	s, err := New(Config{GPXDir: t.TempDir(), ElevationDataset: "srtm30m"})
	if err != nil {
		t.Fatal(err)
	}

	rec := postBatch(t, s, `{"locations":"42.5,-0.4"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "limit exceeded") {
		t.Errorf("body = %s, want the upstream reason", rec.Body)
	}
}

func TestSinglePointLookup(t *testing.T) {
	dem := newFakeDEM(t, false)
	s := newElevationServer(t, dem)

	req := httptest.NewRequest(http.MethodGet, "/elevation?lat=42.5&lon=-0.4&dataset=srtm90m", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if dem.datasets[0] != "srtm90m" {
		t.Errorf("dataset = %q", dem.datasets[0])
	}
	if got := dem.requests[0]; len(got) != 1 || got[0] != "42.5,-0.4" {
		t.Errorf("locations = %v", got)
	}

	got := decodeBatch(t, rec)
	if len(got.Results) != 1 || got.Results[0].Elevation == nil || *got.Results[0].Elevation != 100 {
		t.Errorf("results = %+v", got.Results)
	}
}

func TestSinglePointRejectsBadCoordinates(t *testing.T) {
	dem := newFakeDEM(t, false)
	s := newElevationServer(t, dem)

	for _, target := range []string{
		"/elevation",
		"/elevation?lat=42.5",
		"/elevation?lat=x&lon=y",
		"/elevation?lat=95&lon=0",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}
