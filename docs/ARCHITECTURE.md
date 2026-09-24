# Architecture

A Go server holds the whole app. `web/embed.go` embeds the Vite build with
`go:embed`, so one binary serves the React bundle and the API from the same
origin, and there is nothing to deploy alongside it.

---

## The single binary

```
npm run build   →  web/dist/{index.html,assets/*}
go build ./cmd/overland → //go:embed all:dist → ./overland
```

Build order matters: Go embeds whatever is in `web/dist` at compile time.
`make` runs both in sequence; `go build ./cmd/overland` on its own produces a
working API-only binary that logs that it has no UI.

Two consequences worth knowing:

- **`web/dist/` is gitignored except for a committed `.gitkeep`.** `go:embed`
  fails to compile if its directory does not exist, so the placeholder is what
  keeps a fresh clone buildable. It also means Vite must not empty that
  directory itself — `npm run build` clears `web/dist/assets` first instead.
- **The frontend talks to its own origin.** `VITE_API_BASE` defaults to empty,
  so the bundle requests `/files`, `/gpx/…` and `/elevation` relative to
  wherever it was served. In `npm run dev` those paths are proxied to `:8000`
  by `vite.config.ts`, so the URLs are identical in both modes.

---

## Layout

```
src/
├── App.tsx                     Screens, state, wiring
├── useMcpBridge.ts             Optional MCP browser broker client
├── components/                 Presentational React components
│   ├── ColoredTrack.tsx        Full-resolution track, run-length coloured
│   ├── ElevationProfile.tsx    Profile chart, hover, range selection
│   ├── ExploreScreen.tsx       Search, POIs, area packs and coverage
│   ├── MapLayers.tsx           Base tiles, hillshade pane, terrain controls
│   ├── OfflineAreaPanel.tsx    Drawn-area options, estimate and start
│   ├── OfflineAreasPanel.tsx   Saved-area management and visibility
│   ├── SplashScreen.tsx        Generated topographic intro
│   └── TrackCard.tsx           Library card with route thumbnail
└── lib/                        Pure logic — no React, no DOM*
    ├── types.ts                Shared domain types
    ├── geo.ts                  Distance, elevation stats, smoothing, speed
    ├── gpx.ts                  Parsing and GPX 1.1 writing
    ├── edit.ts                 Trim, split, stages, simplify, smooth
    ├── elevation.ts            Batched DEM lookups
    ├── offline.ts              Runtime policy, status and pack client
    ├── mapStyle.ts             Safe style-resource URL resolution
    ├── fuel.ts                 Spanish official fuel prices
    ├── routing.ts              Broom request and response contract
    ├── surface.ts              Broom annotation classification and summaries
    ├── terrain.ts              Map layers and colour scales
    ├── poi.ts                  Overpass fuel / water / campsite lookups
    ├── mcp.ts                  MCP command decoding and validation
    └── waypointMarkers.ts      Typed marker catalog and GPX-symbol mapping

cmd/overland/
├── main.go                     Minimal urfave/cli entry point
├── serve/                      HTTP server flags and graceful shutdown
├── import/                     Create-only GPX library import
└── util/                       Shared CLI flags
internal/server/
├── server.go                   Chi routes, middleware, embedded-frontend handler
├── files.go                    Track library: list/read/write/upload/delete
├── elevation.go                DEM proxy with upstream-sized chunking
├── elevation_tiles.go          Terrain-RGB tile reader, cache, interpolation
├── cache_store.go              Hashed response bodies, metadata, quota and LRU
├── provider.go                 Read-through policy, revalidation and rate groups
├── data_endpoints.go           Narrow fuel/place/POI adapters
├── broom.go                    Local routing lifecycle, cache and HTTP contract
├── broom_profile.brf           Application-owned motorcycle cost profile
├── broom_enduro.brf            Broom Enduro adaptation with a permit override
├── broom_upgrade.go            Keep readable old graphs live during migration
├── maps.go                     Approved raster and compatible-style adapters
├── packs.go                    Trip-pack estimates, manifests and workers
├── offline.go                  Capabilities, status and management security
└── telemetry.go                Privacy-safe aggregate operational logs
internal/mcp/
├── bridge.go                   Capability-protected active-browser broker
├── server.go                   Official SDK Streamable HTTP server and MCP methods
└── tools.go                    Tool schemas and boundary validation
web/embed.go                    go:embed of the built frontend
web/dist/                       npm run build output (gitignored, embedded)

packaging/                      nfpm config and the example systemd unit
mobile/                         Separate Wails Android module and mobile UI
├── frontend/src/               Map-first React interface; imports shared src/lib
├── host/                       Capability-protected embedding of internal/server
├── cmd/overland/               Android/Wails entry point and native facilities
├── cmd/preview/                Browser preview with the real mobile backend
└── android/                    Checked adaptations of pinned Wails templates
scripts/verify.ts               Logic harness (npm run verify)
gpx/                            Local track library (gitignored)
```

\* `gpx.ts` needs `DOMParser`; the verify harness supplies one via jsdom.

---

## Backend

The Android app uses this same backend through `mobile/host`, served on an
ephemeral loopback port behind a per-launch capability cookie. Its UI is built
separately from `mobile/frontend/src`; GPX maths, editing, routing and provider
clients still come from the shared `src/lib` modules. Wails and the Android
toolchain live in the separate `mobile` Go module. See
[mobile/README.md](../mobile/README.md) for builds, lifecycle and storage.

The CLI uses `urfave/cli`; the HTTP backend uses Chi for routing and middleware,
and the optional MCP endpoint uses the official MCP Go SDK.
The global stack assigns request IDs, recovers panics, compresses eligible
responses, caps concurrent work, supplies security headers and handles explicit
CORS origins. Go 1.27 is the minimum. Track storage uses the
traversal-resistant `os.Root` file APIs, while Broom owns routing graph and
metric access. Keep dependencies beyond Chi, MCP and Broom out of
`internal/server` unless there is a concrete reason to add one.

The listener defaults to `127.0.0.1:8000`, limits header size and read time, and
has bounded idle and header-read deadlines. Browser-originated writes are
accepted only from an exact configured origin, or from matching loopback origins
in the default local setup. Requests without an `Origin` remain valid for CLI
clients, so these checks are not authentication; public deployments still need
an authenticated reverse proxy.

**`files.go`** is a track library over a directory. Every filename arriving from
the network goes through `safeGPXFilename`, which requires a bare `*.gpx` with no
directory component; a name such as `../../etc/passwd` is refused with a 400.
Every filesystem operation then goes through `os.Root`, so a symlink inside the
library cannot escape it. Saves use a rooted temp file and rename, so an
interrupted save cannot leave a truncated track behind. Uploads use exclusive
creation and return 409 rather than replacing an existing filename.

**`elevation.go`** proxies to one of three DEM providers and normalises them
all to the same response shape, so the frontend cannot tell them apart:

| Provider | When | Data |
| --- | --- | --- |
| opentopodata | `-elevation-host` set | whatever you self-host |
| Terrain tiles | default | Terrarium rasters, ~30 m |
| Open-Meteo | `-elevation-tiles=false` | Copernicus DEM GLO-90, 90 m |

`elevation_tiles.go` is the odd one out: rather than asking a service per
point it fetches 256x256 terrain-RGB tiles and reads the raster directly,
interpolating bilinearly between posts. Tiles are deduplicated in flight (a
batch of 100 points usually lands in a handful), held in a 128-tile LRU, and
optionally written to disk — which is what lets elevation work with no
network once a corridor is cached.

The prefetch endpoints warm that cache ahead of use: while planning, the map
reports its bounds once it settles and the server pulls the tiles covering
them in the background. A newer request cancels the previous one, since the
map has moved and the old area is no longer what anyone is looking at. Tile
count grows with the square of how far you zoom out — the planner opens at a
whole-province zoom where the view is thousands of tiles — so a request over
`maxPrefetchTiles` caches the middle of the view and reports that it did.
On-demand lookup still covers whatever the route actually touches.

**The generic offline cache** is separate from the legacy Terrarium tree. Its
keys are SHA-256 values over provider identity, source fingerprint, method and
canonical operation data; raw searches and coordinates never become paths or
routine logs. Bodies and JSON sidecars are written atomically with private
permissions, indexed on startup and bounded by byte and entry quotas. Provider
descriptors decide freshness, stale replay, retention, body/content limits,
pack eligibility and shared request queues. `cache-only` resolves disk/memory
first and returns a typed miss before any outbound transport can run. A
generation-based controller linearises runtime mode changes with transport
starts: switching offline cancels and drains the current generation before the
management response returns, and only startup `auto` mode may create a new
online generation later.

Trip packs are manifests of references to shared cache objects plus explicitly
eligible work such as Terrarium corridor tiles. Public map adapters cannot be
made pack-eligible by a browser request. Interrupted jobs restart as incomplete
and require an explicit new action. Public summaries contain a validated bbox
for rectangular packs but omit route coordinates, cache keys and raw input.

`telemetry.go` periodically snapshots cumulative aggregate counters and current
inventory. It reports cache bytes/hits/misses, response states, outbound status
classes/timing/queue pressure, Terrarium cache use and pack-state counts. It
does not inspect or emit request identities, URLs, provider payloads or pack
identities. The loop belongs to the server context and exits before `Close`
returns.

**`internal/mcp`** is optional and constructed by the `serve` command only when
`--mcp` is set. The official SDK serves standard Streamable HTTP at `/mcp` on a
dedicated loopback listener (`--mcp-addr`) and rejects non-loopback peers. It is
kept off the main listener so a reverse proxy cannot reach it by construction;
only the browser bridge stays there, because the page needs it same-origin. A bounded broker passes
allowlisted commands to the focused browser over private `/mcp/browser/*`
routes and retains its latest state snapshot. Commands are pushed on a
server-sent event stream rather than polled, so an idle tab costs one open
connection instead of several requests a second. The browser half requires a
random per-process capability and loopback Host, Origin and peer checks. The
generic backend receives only an injected `http.Handler`, so disabled runs have
no MCP route or detached MCP lifecycle.

The capability reaches the page as an HttpOnly cookie scoped to
`/mcp/browser`, because `EventSource` cannot send an Authorization header. It
is never readable by page scripts and is never attached to ordinary API calls.
The stream is exempt from the global request throttle: it is held open for the
lifetime of a tab, and a few tabs would otherwise consume the in-flight budget
shared with the rest of the API. Anything wrapping the response writer must
implement `Unwrap`, or `http.ResponseController` cannot flush and commands sit
in a buffer until the connection closes.

Coordinates are parsed and range-checked before they reach an upstream URL, and
requests are chunked to the 100-point limit both services impose, then stitched
back in order. A point the service has no value for comes back `null`, never
`0` — a zero would read as sea level and invent a cliff on the profile.

---

## API

| Route | Purpose |
| --- | --- |
| `GET /files` | Track library listing |
| `GET /gpx/{name}` | Read a track |
| `PUT` / `POST /gpx/{name}` | Write a track (atomic replace) |
| `POST /upload` | Create-only multipart upload, field `file`; returns 409 when the filename exists |
| `DELETE /gpx/{name}` | Delete a track |
| `GET /elevation?lat=&lon=&dataset=` | Single-point DEM lookup |
| `POST /elevation/batch` | `{locations: "lat,lon\|lat,lon…", dataset}` — chunked and stitched |
| `POST /elevation/prefetch` | `{bbox: [s,w,n,e]}` — warm the tile cache for an area, in the background |
| `GET /elevation/prefetch` | Progress of the running prefetch, polled by the UI |
| `GET /config` | Runtime public-service configuration, currently the Nominatim-compatible search URL |
| `GET /fuel` | Persisted Spanish national fuel snapshot |
| `GET /places/search` | Validated, server-rate-limited Nominatim search |
| `POST /pois/search` | Allowlisted POI kind and bounded bbox |
| `POST /routing/broom/route` | Local Road/Dirt/Trail/Enduro route; optional `accessPermit` selects a prepared access-override metric; aligned elevations and OSM annotations |
| `GET /map/raster/{layer}/{z}/{x}/{y}.png` | Passive approved raster cache |
| `/map/openfreemap/*` | Cached OpenFreeMap Liberty source graph, or a configured compatible source |
| `GET /offline/status` | Aggregate cache, provider and job state |
| `GET /offline/routing` | Cheap Broom readiness/progress, installed generations and last storage snapshots; `?summary=1` refreshes advisory metadata-only bytes, `?inventory=1` explicitly verifies ownership/inventory |
| `GET /offline/routing/regions` | Provider country/region hierarchy, public bounds and downloaded/in-use flags; cached-region fallback offline |
| `POST /offline/routing/suggest` | Smallest downloadable region covering a viewport bbox; catalogue lookup only |
| `POST /offline/routing/plan` | Broom acquisition estimate; no PBF or terrain downloads |
| `POST /offline/routing/profile` | Compile and warm a session-only BRF; returns a capability token |
| `POST /offline/routing/profile/release` | Release a session BRF and its temporary metrics |
| `POST /offline/routing/prepare` / `cancel` | Protected routing-data preparation lifecycle |
| `POST /offline/routing/pin` / `prune` | Protected Broom generation retention and cache cleanup |
| `PUT /offline/mode` | Protected runtime transition between `auto` and `cache-only` when startup policy permits |
| `/offline/packs` | Estimate, create, inspect, cancel and delete trip packs |
| `DELETE /offline/cache?scope=…` | Clear unpinned entries in one scope |
| `GET /healthz` | Lightweight liveness check; returns 204 |
| `/mcp/browser/*` | Private browser broker, mounted only with `--mcp`. The agent-facing `/mcp` endpoint is **not** here: it is served by a separate loopback listener so a reverse proxy in front of this one cannot reach it |
| `GET /*` | The React app; unknown paths fall through to it |

Application errors come back as `{"detail": "…"}` with a matching status.
An unavailable cached resource adds `code: "offline_cache_miss"` and `scope`.
Cross-origin access is disabled unless exact origins are configured. There is
no built-in authentication.

---

## Frontend

`src/lib/` holds the domain logic and stays free of React and DOM globals, so
the verify harness can exercise it under Node. The route maths lives here
rather than in Go deliberately: the UI needs it synchronously while dragging a
selection across the elevation profile, and a round trip per interaction would
be visible.

`App.tsx` owns screens and state; everything under `components/` is
presentational.

`useMcpBridge.ts` probes the optional same-origin browser broker. When enabled,
it publishes a bounded state snapshot once per second, receives commands on an
`EventSource` stream, applies them through `App.tsx`'s existing planner/edit
paths, and publishes again before acknowledging. That pre-acknowledgement
publish is retried, unlike the heartbeat: it is what lets an agent read the
state its own tool call produced, so dropping it silently would break the
contract. Commands are applied one at a time, and a redelivered command replays
its stored result rather than running the edit twice — a reconnect can deliver
a duplicate while the original is still in flight. Pure command decoding and
coordinate validation stay in `lib/mcp.ts`. Agent map tracks and markers live in
separate App-level session state: every map renders them, but GPX building,
dirty state, undo history, saves, and downloads never read them. Mode-specific
commands check the active screen; navigation is explicit through `switch_mode`.

Explore owns its area-pack polling and downloaded-coverage visibility. Opening
the area tool enables completed coverage, estimates are aborted and regenerated
when bounds/options change, and the manager scales independently from the map
control stack. Pack summaries are fetched again when Explore remounts, so saved
rectangles survive navigation and reload without browser persistence.

MapLibre runs inside Leaflet through `MapLayers.tsx`. A fetched style graph is
resolved against its style URL before MapLibre sees it, including root-relative
sprite, glyph, source, tile and GeoJSON resources while preserving URL template
tokens. Structured style/source/tile/WebGL failures are dismissible and
deduplicated. They never silently replace OpenFreeMap with a raster provider;
the user must choose a different layer explicitly.

When the Go server serves `index.html`, it injects a meta value containing only
the current offline mode. This closes the pre-`/config` startup window: a
server-started cache-only UI begins without direct providers and uses that
embedded policy as the fallback if config loading fails. A standalone Vite
bundle with no `VITE_API_BASE` has no marker and keeps its deliberate
backend-optional behavior. Supplying `VITE_API_BASE` declares an authoritative
remote backend, so that bundle also starts closed until config succeeds.

Two rules that have each been a bug already:

- **The filename is a track's identity, not its GPX `<name>`.** Cards are
  titled with `fromGpxFilename`, and saving writes back to `track.filename`.
  Deriving a filename from `<name>` forks a second file on every save, and
  tracks that share a `<name>` collapse onto one file.
- **The app must keep working with no backend.** Library, upload and elevation
  calls are best-effort; a dropped GPX file still parses and displays.

---

## Testing

```bash
make check          # everything below, plus go vet, gofmt, tsc, eslint
npm run verify      # logic harness
go test ./internal/...
npm run test:e2e    # Playwright desktop and mobile Chromium projects
```

`npm run verify` is the one that matters when touching parsing, elevation maths
or editing. It parses every file in `gpx/`, round-trips them through the writer,
and asserts the invariants that used to fail silently — attribute-order quirks,
XML escaping, waypoint preservation, noise-floor behaviour, and that simplify
holds distance within 2%.

The library it runs against is your own `gpx/` directory, which is gitignored.
Fixtures are selected by shape (longest track, one carrying waypoints) rather
than by filename, so the harness does not depend on anyone's particular rides;
on a fresh clone the file-backed groups skip themselves and the pure-logic
checks still run.

The Go tests cover the backend's own traps: path-traversal refusal, chunking in
the elevation proxy, result alignment when a DEM returns fewer points than
asked for, the dataset allowlist, provider dialects, no-network transitions and
telemetry privacy. Playwright covers the production browser boundary, including
desktop/mobile vector failure diagnostics, manual offline switching, trip-pack
readiness and downloaded-area restoration.

> [!TIP]
> `go test ./...` also matches a stray Go package inside `node_modules`. Use
> `./internal/...`, or just `make test`.
