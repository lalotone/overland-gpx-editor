<div align="center">

# GPX Editor

**A route planner and track editor for offroad, overlanding and motorbike riding.**

Terrain-first mapping, elevation numbers that do not lie, and a real editor —
shipped as one self-contained binary.

[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![React](https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=black)](https://react.dev)
[![Go dependencies](https://img.shields.io/badge/Go%20dependencies-0-brightgreen)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

</div>

<!--
  A screenshot belongs here. Drop one in docs/ and reference it, e.g.:
  <p align="center"><img src="docs/screenshot.png" alt="GPX Editor" width="900"></p>
-->

---

Most GPX tools are built for road cycling or running. This one is built around
the questions you actually ask before a dirt ride: how steep is that climb, how
high does the route get, what does the terrain look like under it, how far
between fuel stops — and can I cut somebody else's 400 km Wikiloc track down to
the part I want.

- 🗺️ **Terrain-first mapping** — contours, hillshade relief, satellite, and live
  ground elevation under the cursor
- 📐 **Honest numbers** — elevation gain that filters GPS noise instead of
  inflating totals by 10–50%, and slope measured over real distance
- ✂️ **A real editor** — crop, split, day-stage, simplify and repair elevation
  on any track you load
- 🏍️ **Motorbike routing** — Valhalla `motorcycle` costing with road / dirt /
  trail profiles, not a repurposed bicycle model
- 📦 **One binary** — the React app is embedded in the Go server with
  `go:embed`. Copy ~7 MB to a machine and run it: no runtime, no dependencies,
  no container needed

**Contents** — [Quick start](#quick-start) · [Configuration](#configuration) ·
[Features](#features) · [Accuracy](#how-the-numbers-are-computed) ·
[Architecture](#architecture) · [API](#api) · [Development](#development) ·
[External services](#external-services) · [Limitations](#known-limitations) ·
[License](#license)

---

## Quick start

**To build:** Go 1.22+ (developed on 1.26) and Node 18+ (developed on 24).
**To run:** nothing at all.

```bash
git clone https://github.com/lalotone/overland-gpx-editor.git
cd overland-gpx-editor
npm install
make            # npm run build + go build → ./gpx-editor
./gpx-editor    # http://localhost:8000
```

That is the whole app — frontend, API and track library in one process. Tracks
live as plain `.gpx` files in `gpx/` next to the binary, so your library stays
readable by every other tool you own.

```
Usage of ./gpx-editor:
  -addr string                address to listen on (default ":8000")
  -gpx-dir string             directory holding the track library (default "gpx")
  -elevation-host string      self-hosted opentopodata-style DEM service;
                              empty uses the public Open-Meteo API
  -elevation-dataset string   DEM dataset for -elevation-host (default "srtm30m")
```

> [!NOTE]
> The frontend build output is not committed, so `go install` on its own
> produces an **API-only** binary — it starts, says so, and serves no UI. Build
> with `make` (or run `npm run build` before `go build`) to get the app.

Cross-compiling needs no C toolchain: `make cross` writes Linux, macOS and
Windows binaries to `build/`.

### Running it as a service

```ini
# /etc/systemd/system/gpx-editor.service
[Service]
ExecStart=/opt/gpx-editor/gpx-editor -addr 127.0.0.1:8000 -gpx-dir /srv/tracks
Environment=ELEVATION_HOST=http://dem.lan:30110
Restart=on-failure
User=gpx

[Install]
WantedBy=multi-user.target
```

There is no authentication and CORS is open — put it behind a reverse proxy
with auth, or keep it on a trusted network. See
[Known limitations](#known-limitations).

### Working on the frontend

```bash
./gpx-editor &         # backend on :8000
npm run dev            # http://localhost:5173, with HMR
```

Vite proxies `/files`, `/gpx`, `/upload` and `/elevation` to `:8000`, so the app
uses the same URLs in dev as it does inside the binary. It also works with no
backend at all — drag a GPX file onto it — but the saved-track library and
batched elevation lookups need the server running.

### Elevation

Out of the box the backend proxies to [Open-Meteo](https://open-meteo.com/en/docs/elevation-api),
which is public, keyless and needs no setup. That covers ground elevation under
the cursor, elevation for newly planned routes, and "Refetch from DEM".

Open-Meteo serves **Copernicus DEM GLO-90**. Ninety-metre postings smooth out
exactly the gradients that matter on a trail (see
[docs/ACCURACY.md](docs/ACCURACY.md)), so if you care about slope accuracy,
self-host a 30 m dataset and point the binary at it:

```bash
./gpx-editor -elevation-host http://dem.lan:30110 -elevation-dataset srtm30m
```

Anything [opentopodata](https://www.opentopodata.org/)-compatible works:

```
GET  <host>/v1/<dataset>?locations=42.1,-0.4|42.2,-0.5
→    { "results": [ { "elevation": 512.0 }, { "elevation": 498.5 } ] }
```

Setting `-elevation-host` switches the proxy over to it; leaving it empty keeps
Open-Meteo. Either way, tracks that already carry `<ele>` data display and
analyse correctly with no elevation service reachable at all.

---

## Configuration

Every value has a working default; copy `.env.example` to `.env` to change any
of them.

| Variable | Side | Default | Purpose |
| --- | --- | --- | --- |
| `ADDR` | backend | `:8000` | Listen address (flag: `-addr`) |
| `GPX_DIR` | backend | `gpx` | Track library directory (flag: `-gpx-dir`) |
| `ELEVATION_HOST` | backend | *(empty — uses Open-Meteo)* | Self-hosted opentopodata-style DEM (flag: `-elevation-host`) |
| `ELEVATION_DATASET` | backend | `srtm30m` | Dataset for `ELEVATION_HOST` when a request names none (flag: `-elevation-dataset`) |
| `VITE_API_BASE` | frontend | *(empty — same origin)* | Only needed to point the app at a backend on another host |
| `VITE_DEV_API_TARGET` | frontend | `http://localhost:8000` | Where `npm run dev` proxies the API paths |
| `VITE_ELEVATION_API` | frontend | *(empty — disabled)* | Optional opentopodata address the browser calls directly if the backend proxy fails |
| `VITE_ELEVATION_DATASET` | frontend | `srtm30m` | DEM dataset name |

Backend flags win over the environment. The `VITE_` values are baked into the
bundle at `npm run build` time, not read by the binary at startup.

Elevation requests go through the backend, which owns the choice of DEM source;
`VITE_ELEVATION_API` only matters for installs that keep a browser-reachable
DEM as a second chance.

For a self-hosted service, `srtm90m` works but its 90 m postings smooth out
exactly the gradients that matter on a trail. Prefer `srtm30m` or a 30 m
Copernicus dataset.

---

## Features

Full detail in **[FEATURES.md](FEATURES.md)**. In brief:

**Terrain** — OpenStreetMap / OpenTopoMap (contours) / CyclOSM (surface and
grade) / Esri satellite / Esri shaded relief, plus a hillshade overlay with
adjustable strength that works over any base. Track colouring by gradient or by
altitude on a hypsometric ramp. Live ground elevation under the cursor.

**Analysis** — distance, min/max altitude, gain/loss, moving time and average
moving speed from recorded timestamps, longest gap without fuel.

**Editing** — select a range on the elevation profile, then crop or split;
day-stage splitting; reverse; Douglas-Peucker simplify to a point budget;
elevation smoothing; refetch elevation from the DEM. All undoable.

**Planning** — click waypoints, drag to adjust, route with motorbike profiles,
place search, POI layers (fuel / water / campsites) from Overpass.

**Files** — tolerant parsing of `<trk>`, `<rte>` and `<wpt>` across exporter
quirks; GPX 1.1 output with proper namespaces, escaping, timestamps and
waypoints preserved.

---

## How the numbers are computed

Worth reading before you trust any figure the app shows:
**[docs/ACCURACY.md](docs/ACCURACY.md)**.

The short version — elevation gain is *not* a raw sum of positive differences.
That method over-reports by 10–50% on recorded tracks because it accumulates
GPS and barometric jitter. Elevation is smoothed over a 60 m distance window,
then accumulated with a 3 m noise floor using direction hysteresis. Slope is
always measured over a real distance denominator, never between two adjacent
points.

---

## Architecture

A Go server holds the whole app. `web/embed.go` embeds the Vite build with
`go:embed`, so the binary serves the React bundle and the API from one origin
and there is nothing to deploy alongside it.

```
src/
├── App.tsx                     Screens, state, wiring
├── components/                 Presentational React components
│   ├── ColoredTrack.tsx        Full-resolution track, run-length coloured
│   ├── ElevationProfile.tsx    Profile chart, hover, range selection
│   ├── MapLayers.tsx           Base tiles, hillshade pane, terrain controls
│   ├── SplashScreen.tsx        Generated topographic intro
│   └── TrackCard.tsx           Library card with route thumbnail
└── lib/                        Pure logic — no React, no DOM*
    ├── types.ts                Shared domain types
    ├── geo.ts                  Distance, elevation stats, smoothing, speed
    ├── gpx.ts                  Parsing and GPX 1.1 writing
    ├── edit.ts                 Trim, split, stages, simplify, smooth
    ├── elevation.ts            Batched DEM lookups
    ├── routing.ts              Valhalla costing and fallbacks
    ├── terrain.ts              Map layers and colour scales
    └── poi.ts                  Overpass fuel / water / campsite lookups

main.go                         Flags, wiring, graceful shutdown
internal/server/
├── server.go                   Routes, CORS, embedded-frontend handler
├── files.go                    Track library: list/read/write/upload/delete
└── elevation.go                DEM proxy with upstream-sized chunking
web/embed.go                    go:embed of the built frontend
web/dist/                       npm run build output (gitignored, embedded)

scripts/verify.ts               Logic harness (npm run verify)
gpx/                            Local track library (gitignored)
```

\* `gpx.ts` needs `DOMParser`; the verify harness supplies one via jsdom.

The Go side has **no third-party dependencies**: routing is `http.ServeMux`
with Go 1.22 method patterns, and `go.mod` has an empty require block. The
route maths stays in TypeScript, where the UI can use it directly and the
verify harness can exercise it against real files.

---

## API

| Route | Purpose |
| --- | --- |
| `GET /files` | Track library listing |
| `GET /gpx/{name}` | Read a track |
| `PUT` / `POST /gpx/{name}` | Write a track (atomic replace) |
| `POST /upload` | Multipart upload, field `file` |
| `DELETE /gpx/{name}` | Delete a track |
| `GET /elevation?lat=&lon=&dataset=` | Single-point DEM lookup |
| `POST /elevation/batch` | `{locations: "lat,lon\|lat,lon…", dataset}` — chunked to the upstream limit and stitched back in order |
| `GET /*` | The React app; unknown paths fall through to it |

Filenames must be a bare `*.gpx` with no directory component — anything else is
a 400 before it reaches the filesystem. Errors come back as
`{"detail": "…"}` with a matching status.

---

## Development

```bash
make               # frontend + binary
make run           # build, then serve on :8000
make test          # go test + npm run verify
make check         # tests, go vet, gofmt, tsc, eslint
make cross         # release binaries for linux/darwin/windows

npm run dev        # frontend dev server with HMR
npm run verify     # logic checks against the real files in gpx/
go test ./internal/...
```

`npm run verify` is the one that matters when touching parsing, elevation maths
or editing. It parses every file in `gpx/`, round-trips them through the
writer, and asserts the invariants that used to fail silently — attribute-order
quirks, XML escaping, waypoint preservation, noise-floor behaviour, and that
simplify holds distance within 2%.

The Go tests cover the backend's own traps: path-traversal refusal, the
upstream chunking in the elevation proxy, result alignment when the DEM returns
fewer points than asked for, and the dataset allowlist.

There is no browser test setup. UI changes need looking at by hand.

> [!TIP]
> `go test ./...` also matches a stray Go package inside `node_modules`. Use
> `./internal/...`, or just `make test`.

---

## External services

All are public and keyless. Attribution is rendered on the map by Leaflet.

| Service | Used for | Notes |
| --- | --- | --- |
| [OpenStreetMap](https://www.openstreetmap.org/copyright) tiles | Base map | Subject to the [tile usage policy](https://operations.osmfoundation.org/policies/tiles/) — fine for personal use, not for bulk downloading |
| [OpenTopoMap](https://opentopomap.org) | Contour base map | CC-BY-SA; low-volume use only |
| [CyclOSM](https://www.cyclosm.org) | Surface/grade base map | Community-run tile server |
| Esri ArcGIS | Satellite, relief, hillshade | Attribution required |
| [Valhalla](https://valhalla1.openstreetmap.de) | Routing | Public demo instance; may lack `motorcycle` costing, in which case the app falls back and says so |
| [OSRM](https://router.project-osrm.org) | Routing fallback | Public demo instance, car profile only |
| [Nominatim](https://nominatim.org) | Place search | [Usage policy](https://operations.osmfoundation.org/policies/nominatim/): max 1 request/second |
| [Overpass](https://overpass-api.de) | Fuel / water / campsite POIs | Shared public instance; queries are on-demand only |
| [Open-Meteo](https://open-meteo.com/en/docs/elevation-api) | Elevation (default) | Keyless, 100 points per request; Copernicus DEM GLO-90. Replaced by `-elevation-host` when you self-host a DEM |

These are shared community resources running on donated infrastructure. The app
is built for personal-scale use and does not cache tiles or throttle
aggressively. If you point it at heavy or automated workloads, self-host the
services first.

---

## Known limitations

- **No surface breakdown yet** — "% unpaved", `tracktype` / `smoothness`
  colouring. This is the biggest remaining gap for offroad planning; it needs
  Valhalla `trace_attributes` or an Overpass tag join.
- **No offline tile caching.** Plan at home, because the map will be blank in
  the field.
- **No access warnings** — `access=private`, gates and seasonal closures are
  not flagged.
- **Desktop-shaped.** The creation screen assumes a wide window.
- **Distance is 2D.** Horizontal only; the elevation component is not added, so
  steep tracks read very slightly short. See [docs/ACCURACY.md](docs/ACCURACY.md).
- **The backend has no authentication**, and CORS is wide open. It reads,
  writes and deletes files in `gpx/`. Bind it to a trusted network only.

---

## Contributing

Issues and pull requests are welcome. Before opening one:

1. Run `make check` — it has to be green.
2. Read [docs/ACCURACY.md](docs/ACCURACY.md) first if you are touching
   distance, elevation or slope. The methodology there is deliberate, and
   several obvious "simplifications" are the bugs it exists to prevent.
3. Keep `src/lib/` free of React and DOM globals, and the Go backend free of
   third-party dependencies.

[AGENTS.md](AGENTS.md) documents the same conventions in the form coding agents
expect.

---

## Documentation

| Document | Contents |
| --- | --- |
| [FEATURES.md](FEATURES.md) | Complete feature reference |
| [docs/ACCURACY.md](docs/ACCURACY.md) | How distance, elevation and slope are computed, and what to trust |
| [AGENTS.md](AGENTS.md) | Conventions and workflow for coding agents |
| [.env.example](.env.example) | Every configuration variable |

---

## License

[MIT](LICENSE) © lalotone

Map data © OpenStreetMap contributors, ODbL. Tiles and routing come from the
third-party services listed above, each under its own terms.
