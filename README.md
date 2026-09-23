<div align="center">

# GPX Editor

**A route planner and track editor for offroad, overlanding and motorbike riding.**

Terrain-first mapping, elevation numbers that do not lie, and a real editor —
shipped as one self-contained binary.

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![React](https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=black)](https://react.dev)
[![Go dependencies](https://img.shields.io/badge/Go%20dependencies-4-brightgreen)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

</div>

<p align="center">
  <img src="docs/screenshot.webp" width="900"
       alt="Planning a dirt route in the Pyrenees: contour basemap with hillshade, the route coloured by surface, and a sidebar showing distance, elevation, a 100% unpaved surface breakdown and the route points.">
</p>

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
- 🏍️ **Motorbike routing** — embedded Broom routing with road / dirt / trail / enduro
  profiles, OSM surface annotations and no remote route service
- 🧭 **Prepared for dead zones** — viewed resources and trip packs survive a
  restart, with a strict cache-only mode that makes no upstream requests
- 📦 **One binary** — the React app is embedded in the Go server with
  `go:embed`. Copy ~20 MB to a machine and run it: no runtime, no dependencies,
  no container needed

---

## Install

Grab the archive for your platform from
[Releases](https://github.com/lalotone/overland-gpx-editor/releases), unpack
it, and run the binary. It serves the whole app — frontend, API and track
library — on <http://localhost:8000>. There is nothing else to install.

```bash
tar xzf gpx-editor-linux-amd64.tar.gz
cd gpx-editor-linux-amd64
./overland serve
```

On Debian, Ubuntu, Fedora or RHEL there are packages instead:

```bash
sudo apt install ./gpx-editor_0.1.0_amd64.deb     # or
sudo dnf install ./gpx-editor-0.1.0-1.x86_64.rpm
```

They install `/usr/bin/overland` and leave an example systemd unit in
`/usr/share/doc/gpx-editor/`. It is deliberately not registered with systemd:
the user, the library directory and the listen address are yours to choose,
and a package that silently starts serving on `:8000` is not a nice surprise.

The binaries are unsigned, so both desktop platforms will object once:

- **macOS** — Gatekeeper quarantines the download. Clear it with
  `xattr -dr com.apple.quarantine ./overland`, or right-click → Open.
- **Windows** — SmartScreen warns about an unrecognised app. More info →
  Run anyway.

Each release ships `SHA256SUMS`; check a download with
`sha256sum -c SHA256SUMS --ignore-missing`.

### Or build it

**To build:** Go 1.27+ and Node 18+. **To run:** nothing at all.

```bash
git clone https://github.com/lalotone/overland-gpx-editor.git
cd overland-gpx-editor
make            # installs npm deps, builds the frontend, builds the binary
./overland serve    # http://localhost:8000
```

That is the whole app — frontend, API and track library in one process. Tracks
live as plain `.gpx` files in `$XDG_DATA_HOME/overland/gpx` (normally
`~/.local/share/overland/gpx`), so your library stays readable by every other
tool you own and the rest of the app data has room alongside it.

The CLI has two commands:

```bash
# Start the web app and API.
./overland serve [options]

# Add one or more files to the library without replacing existing tracks.
./overland import [--gpx-dir DIR] FILE...
```

Run `./overland serve --help` for server and elevation options, or
`./overland import --help` for import usage. Both commands read `GPX_DIR` and
default to `$XDG_DATA_HOME/overland/gpx` (normally
`~/.local/share/overland/gpx`).

Agents can control the open planner and track editor through an optional local
MCP Streamable HTTP endpoint:

```bash
./overland serve --mcp                        # http://127.0.0.1:8009/mcp
./overland serve --mcp --mcp-addr 127.0.0.1:9009
```

The MCP endpoint listens on its own loopback address rather than on `--addr`,
so a reverse proxy in front of the app cannot reach it. Behind a proxy, run the
server with `--behind-proxy`: it withdraws the implicit loopback trust that a
proxy would otherwise hand to every remote client, and offline management then
requires `OFFLINE_ADMIN_TOKEN`.

It exposes route controls, typed GPX waypoints, editable track geometry,
explicit planner/editor/Explore switching, and session-only map annotations,
plus library opening, POI loading, and an `overland://view` resource containing
the current map and planning context. The endpoint uses the standard MCP
Streamable HTTP transport, rejects non-loopback clients, and never saves an
edit automatically. See **[docs/MCP.md](docs/MCP.md)** for setup, tools and the
local trust boundary.

`make cross` writes Linux, macOS and Windows binaries to `build/`, and
`make dist` archives them with `.deb`/`.rpm` packages and checksums the way a
release does. None of it needs a C toolchain, a container or a macOS runner,
since nothing here uses cgo — `make packages` does want
[nfpm](https://nfpm.goreleaser.com) on `PATH`.

> [!NOTE]
> The frontend build output is not committed, so
> `go install ./cmd/overland` produces an **API-only** binary. Build with
> `make` to get the app.

**Working on the frontend:** run `./overland serve` and `npm run dev` side by side.
Vite proxies the API paths to `:8000`, so the app uses the same URLs in dev as
it does inside the binary.

**Running it as a service:** the server listens on loopback by default and has
no authentication. Put it behind a reverse proxy with auth before binding it to
a public interface. Browser writes are loopback-only by default; pass one or
more exact `--allowed-origin https://planner.example.com` values for every
non-loopback UI origin, including a public same-origin deployment. Origin
checks reduce browser abuse but do not authenticate command-line or network
clients. Offline management on any non-loopback deployment additionally needs
`--trusted-ui-origin https://planner.example.com` (or an admin bearer token).

---

## Elevation

Out of the box the backend reads elevation from **terrain tiles** — ~30 m
Terrarium rasters, cached in `$XDG_CACHE_HOME/overland/tiles` (normally
`~/.cache/overland/tiles`). Nothing to configure and no per-point API quota to
run into.

Tiles are read locally rather than asked for a point at a time, which is why
they are the default: a 95 km route needs 27 tiles (2.6 MB) and about 5
seconds cold, then costs nothing, and a cached area keeps working with no
signal. The trade is bandwidth — tiles are ~100 KB each against a few KB of
JSON for the same route — and opening the planner zoomed out caches ~22 MB
for the region you are looking at.

While you plan, tiles for the visible area download in the background with a
progress readout on the map; pan somewhere else and it follows. Zoomed far
out the view runs to thousands of tiles, so only its middle is cached and the
readout says so. Elevation still works everywhere either way — anything not
cached is fetched on demand.

Two alternatives:

```bash
# A DEM you already run. Takes precedence over tiles.
./overland serve --elevation-host http://dem.lan:30110 --elevation-dataset srtm30m

# The free non-commercial Open-Meteo API. No downloads, but Copernicus 90 m
# and a daily request quota — 90 m postings smooth out exactly the gradients
# that matter on a trail. One Pyrenean point reads 1539 m from it and 1920 m
# from 30 m tiles.
./overland serve --elevation-tiles=false
```

Whichever you use, tracks that already carry `<ele>` data display and analyse
correctly with no elevation service reachable at all.

## Offline use

The Go server keeps policy-permitted provider responses in
`$XDG_CACHE_HOME/overland/responses` (normally
`~/.cache/overland/responses`). Opening a GPX automatically prepares a bounded
pack for that route. The **Offline** pill appears directly below Terrain and
shows live readiness for the vector map, elevation, fuel, water and campsites;
open it for per-resource progress and item counts. Recent prepared routes remain
pinned; at the manifest limit the oldest completed automatic pack is released.

Explore can also download a rectangular area without a GPX. Draw the bounds and
the estimate updates automatically before download. Completed bounds are shown
as light coverage rectangles and restored after reload; the downloaded-area
manager can hide them, inspect progress, cancel work or delete a saved area.

In the default `auto` mode, **Work offline** switches the running server and UI
to deterministic no-network operation immediately; **Go online** re-enables
provider traffic. Starting with `--offline-mode cache-only` is an operator lock
and cannot be overridden by the browser. Cached data is served with its source
and cache date; unknown resources fail immediately without bypassing the server
or stopping GPX editing. It does not make the web UI available when the browser
cannot reach the Go server itself.

Routing is separate from response-cache trip packs. Broom stores OSM extracts,
elevation sources, prepared graphs and profile metrics in
`$XDG_CACHE_HOME/overland/routing` (normally `~/.cache/overland/routing`).
Planner and Explore use Broom's region suggestions to find local routing data
for the visible map, explicitly labelling partial coverage. Open the
**Offline routing** map pill to see available acquisition estimates and download the
suggested region or use an installed copy. The same pill reveals preparation
progress and cancellation; it keeps showing tile totals and progress when collapsed. Preparation
runs in the background, and completed route queries are entirely local. Updates,
pinning and pruning remain available through the management API. A strict
cache-only server can open installed regions but never downloads missing data.

For unattended startup, use:

```bash
./overland serve --routing-region spain/aragon --routing-prepare
```

`--routing-graph` opens a trusted application-built Broom graph instead of one
managed by region; use this for cross-border union graphs. Routing has no public
provider fallback. Without prepared data it reports that routing data is needed
while GPX loading and editing continue to work.

Motorcycle profiles require explicit motor permission on non-motor paths by
default. Enable **Restricted access** when you have a permit for restricted
ways: it allows access-restricted roads and supported paths, including private
access and gates/chains you have permission to pass. This applies to the whole
planned route, defaults off, and resets when you clear the planner. One-way rules,
turn restrictions and impassable obstacles still apply.

**Enduro** uses an Overland adaptation of Broom's road-registered motorcycle BRF,
with the same default preferences and the permit option. The **Upload session BRF** icon beside the profiles
lets you prepare a custom profile for the currently open graph. It is temporary:
no library file or browser storage is written, and it is released when removed,
on page exit, or after two hours. Switching routing regions requires a new upload.
Uploaded BRFs define their own access policy; the permit checkbox is disabled
for them.

After a Broom upgrade, the active managed region updates automatically when
online. Its previous graph remains usable while it rebuilds, and **Offline
routing** shows progress. In cache-only mode the update waits until you return
online. Failed or paused updates can be resumed from the same panel.

OpenFreeMap is cached through the backend and supports bounded trip-pack
prefetch by default. Attribution remains visible, and the source can be
overridden or bulk fetching disabled at startup. OSM, OpenTopoMap and CyclOSM
are cached only as they are viewed and are never area-prefetched. Esri live
layers remain browser-direct and are not stored by the server. An OpenFreeMap
failure produces a dismissible structured warning and never silently switches
the selected map to another provider. See
**[docs/OFFLINE.md](docs/OFFLINE.md)** for provider and privacy details.

The server prints aggregate cache, outbound, elevation-tile and pack counters in
one aligned, human-readable table per minute. `--stats-log-interval 0` disables
it. The summaries contain no URLs, searches, coordinates, cache keys, response
bodies or pack identities.

---

## Configuration

Every value has a working default; copy `.env.example` to `.env` to change any.
Backend flags win over the environment; the `VITE_` values are baked into the
bundle at build time.

| Variable | Side | Default | Purpose |
| --- | --- | --- | --- |
| `ADDR` | backend | `127.0.0.1:8000` | Listen address; use a public interface only behind authentication |
| `MCP` | backend | `off` | Enable local Streamable HTTP MCP control on its own loopback listener |
| `MCP_ADDR` | backend | `127.0.0.1:8009` | Address for the MCP endpoint; refuses anything but loopback, and is never behind the proxy |
| `BEHIND_PROXY` | backend | `off` | The server runs behind a reverse proxy; withdraws implicit loopback trust, so management needs `OFFLINE_ADMIN_TOKEN` and the relay needs `ALLOWED_ORIGINS` |
| `GPX_DIR` | backend | `$XDG_DATA_HOME/overland/gpx` (`~/.local/share/overland/gpx`) | Track library directory |
| `NOMINATIM_URL` | backend | `https://nominatim.openstreetmap.org` | Nominatim-compatible place-search service exposed through runtime config |
| `ALLOWED_ORIGINS` | backend | *(empty)* | Comma-separated exact browser origins allowed to call the API |
| `OFFLINE_CACHE_DIR` | backend | `$XDG_CACHE_HOME/overland/responses` | Persistent provider cache; an explicitly empty value disables persistence |
| `OFFLINE_CACHE_MAX_BYTES` | backend | `1GiB` | Generic cache quota, including metadata |
| `OFFLINE_CACHE_MAX_ENTRIES` | backend | `100000` | Generic cache entry/inode guard |
| `OFFLINE_MODE` | backend | `auto` | `auto` or strict no-outbound `cache-only` |
| `STATS_LOG_INTERVAL` | backend | `1m` | Privacy-safe aggregate cache/outbound log interval; `0` disables it |
| `UPSTREAM_CONTACT` | backend | project URL | Contact included in the outbound User-Agent |
| `TRUSTED_UI_ORIGIN` | backend | *(loopback UI only)* | Exact non-loopback UI origin allowed to manage offline data |
| `OFFLINE_ADMIN_TOKEN` | backend | *(empty)* | Bearer token for non-loopback management clients |
| `ROUTING_CACHE_DIR` | backend | `$XDG_CACHE_HOME/overland/routing` | Broom sources, graphs and metrics; empty disables routing |
| `ROUTING_REGION` | backend | *(empty)* | Canonical Broom region to reopen or prepare |
| `ROUTING_GRAPH` | backend | *(empty)* | Trusted application-owned Broom graph, including cross-border unions |
| `ROUTING_PREPARE` / `ROUTING_UPDATE` | backend | `off` | Prepare or explicitly refresh `ROUTING_REGION` at startup |
| `ROUTING_JOBS` / `ROUTING_CONCURRENCY` / `ROUTING_TIMEOUT` | backend | `2` / `4` / `45s` | Preparation parallelism and route-query bounds |
| `ROUTING_INDEX_URL` / `ROUTING_METADATA_INDEX_URL` | backend | Broom defaults | Optional region catalogue mirrors |
| `ROUTING_PBF_BASE_URL` / `ROUTING_DEM_BASE_URL` | backend | Broom defaults | Optional OSM extract and DEM mirrors |
| `OVERPASS_URL` / `FUEL_URL` | backend | public services | Startup-only data-service overrides |
| `OPENFREEMAP_URL` | backend | OpenFreeMap Liberty style | OpenFreeMap-compatible source eligible for persistent proxying |
| `OPENFREEMAP_ALLOW_BULK` | backend | `on` | Permit bounded trip-pack fetching from the configured source |
| `ELEVATION_TILES` | backend | `on` | Read elevation from ~30 m terrain tiles; `0` falls back to Open-Meteo |
| `ELEVATION_TILE_ZOOM` | backend | `13` | Tile zoom — higher is finer and heavier |
| `ELEVATION_TILE_CACHE` | backend | `$XDG_CACHE_HOME/overland/tiles` (`~/.cache/overland/tiles`) | Where tiles are kept, so elevation works offline |
| `ELEVATION_TILE_CACHE_MAX_BYTES` | backend | `1GiB` | Separate legacy Terrarium cache quota |
| `ELEVATION_HOST` | backend | *(empty)* | Self-hosted opentopodata-style DEM. Takes precedence over tiles |
| `ELEVATION_DATASET` | backend | `srtm30m` | Dataset for `ELEVATION_HOST` |
| `VITE_API_BASE` | frontend | *(empty — same origin)* | Points the app at an authoritative remote backend; direct fallbacks stay closed until its config loads |
| `VITE_DEV_API_TARGET` | frontend | `http://localhost:8000` | Where `npm run dev` proxies the API |
| `VITE_ELEVATION_API` | frontend | *(empty — disabled)* | Optional direct DEM call if the proxy fails |
| `VITE_ELEVATION_DATASET` | frontend | `srtm30m` | DEM dataset name |

---

## Features

Full detail in **[FEATURES.md](FEATURES.md)**. In brief:

**Terrain** — OpenFreeMap vector maps / OpenTopoMap / CyclOSM / Esri satellite
and shaded relief, plus a hillshade overlay that works over any base. Track
colouring by gradient or altitude. Live ground elevation under the cursor.

**Analysis** — distance, min/max altitude, gain/loss, moving time and average
moving speed from recorded timestamps, longest gap without fuel.

**Editing** — select a range on the elevation profile, then crop or split;
day-stage splitting; reverse; Douglas-Peucker simplify to a point budget;
elevation smoothing; refetch elevation from the DEM. All undoable.

**Planning** — click waypoints, drag to adjust, route with motorbike profiles,
place search, POI layers (fuel / water / campsites).

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

## Development

```bash
make           # install deps, build the frontend, build the binary
make run       # the above, then serve on :8000
make test      # go test + npm run verify
make check     # tests plus go vet, gofmt, tsc, eslint
make cross     # release binaries for linux/darwin/windows
make packages  # .deb and .rpm (needs nfpm)
make dist      # all of the above, archived with SHA256SUMS
npm run dev    # frontend dev server with HMR (needs ./overland serve running)
npx playwright install chromium  # once, for browser tests
npm run test:e2e                 # desktop/mobile map and offline UI flows
```

Pushing a `v*` tag builds and publishes a release; every push and pull request
runs `make check`. Both live in [.github/workflows](.github/workflows).

Targets that need the npm toolchain install it themselves, so `make` works on a
bare clone.

How it fits together, the file tree and the HTTP API are in
**[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)**.

---

## External services

All are public and keyless. Attribution for map, OSM-derived and elevation data
is rendered on the map by Leaflet.

| Service | Used for |
| --- | --- |
| [OpenFreeMap](https://openfreemap.org) | Vector base map (OpenStreetMap data) |
| [OpenStreetMap](https://www.openstreetmap.org/copyright) | Raster library thumbnails and non-WebGL fallback — see the [tile usage policy](https://operations.osmfoundation.org/policies/tiles/) |
| [OpenTopoMap](https://opentopomap.org) | Contour base map (CC-BY-SA, low volume only) |
| [CyclOSM](https://www.cyclosm.org) | Surface/grade base map |
| Esri ArcGIS | Satellite, relief, hillshade (attribution required; World Shaded Relief retires March 2028) |
| [Broom](https://code.rbel.co/rubiojr/broom) | Embedded local routing over explicitly prepared OpenStreetMap and DEM data |
| [Nominatim](https://nominatim.org) | Place search, throttled to one request/second and cached; switchable with `NOMINATIM_URL` |
| [Overpass](https://overpass-api.de) | User-triggered fuel / water / campsite POIs |
| [Spanish fuel-price feed](https://datos.gob.es/es/catalogo/e05068001-precio-de-carburantes-en-las-gasolineras-espanolas) | Official national snapshot, persistently cached with its publication time |
| [AWS Terrain Tiles](https://registry.opendata.aws/terrain-tiles/) | Elevation by default — mixed-source data with [source-specific attribution](https://github.com/tilezen/joerd/blob/master/docs/attribution.md) |
| [Open-Meteo](https://open-meteo.com/en/docs/elevation-api) | Elevation with `-elevation-tiles=false`; free endpoint is non-commercial and quota-limited |

These are shared community resources. Nominatim requests are serialized to its
one-request-per-second limit, and repeated place searches are cached. Public
raster basemap tiles are never prefetched; OSM, OpenTopoMap
and CyclOSM viewed tiles may use the bounded server cache for their
provider-permitted lifetime. Bounded OpenFreeMap vector packs are the explicit
supported exception.
For high-traffic, commercial or automated workloads, use contracted or
self-hosted services instead.

Backend request queues are process-wide, so several tabs share provider limits;
standalone direct-provider fallbacks retain their per-browser queues. A public
deployment still needs authentication and must publish the operator contact
required by provider terms.

---

## Known limitations

- **No surface breakdown yet** — "% unpaved", `tracktype` / `smoothness`
  colouring. The biggest remaining gap for offroad planning.
- **No public raster basemap area downloads.** OpenFreeMap supports bounded
  vector packs; OSM, OpenTopoMap and CyclOSM remain viewed-tile caches only.
- **No access warnings** — `access=private`, gates and seasonal closures are
  not flagged.
- **Desktop-shaped.** The creation screen assumes a wide window.
- **Distance is 2D**, so steep tracks read very slightly short.
- **No built-in authentication.** The backend reads, writes and deletes files
  in the configured track library. It defaults to loopback and rejects
  untrusted browser-originated writes, but public deployments still need an
  authenticated reverse proxy.

---

## Contributing

Issues and pull requests are welcome. Before opening one:

1. Run `make check` — it has to be green.
2. Read [docs/ACCURACY.md](docs/ACCURACY.md) first if you are touching
   distance, elevation or slope. The methodology there is deliberate, and
   several obvious "simplifications" are the bugs it exists to prevent.
3. Keep `src/lib/` free of React and DOM globals, and keep Go HTTP dependencies
   limited to the existing Chi routing stack unless there is a concrete need.

| Document | Contents |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | How it fits together, file tree, HTTP API |
| [docs/ACCURACY.md](docs/ACCURACY.md) | How distance, elevation and slope are computed |
| [docs/MCP.md](docs/MCP.md) | MCP setup, tools, view resource and security |
| [FEATURES.md](FEATURES.md) | Complete feature reference |
| [AGENTS.md](AGENTS.md) | Conventions and workflow for coding agents |
| [.env.example](.env.example) | Every configuration variable |

---

## License

[MIT](LICENSE) © lalotone

Map data © OpenStreetMap contributors, ODbL. Tiles and routing come from the
third-party services listed above, each under its own terms.
