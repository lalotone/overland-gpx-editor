# GPX Editor - Agent Guidelines

A GPX route planner and editor aimed at offroad / overlanding / motorbike use.
Priorities in that order: correct numbers, terrain legibility, then polish.

Read [README.md](README.md) for setup and [docs/ACCURACY.md](docs/ACCURACY.md)
before touching anything that computes distance, elevation or slope — the
methodology there is deliberate and several obvious "simplifications" are the
bugs it exists to prevent.

## Development Workflow

### Commit Strategy
- **Commit after every meaningful change** to enable easy rollback
- Use descriptive commit messages that explain the "why" not just the "what"
- Keep commits focused on a single concern

### Checks before committing
```bash
make check          # tsc, eslint, go vet, gofmt, and every test
```

or individually:
```bash
npx tsc --noEmit         # type check
npm run verify           # logic checks against the real files in ./gpx
npm run lint             # eslint
go test ./internal/...   # backend tests — NOT ./..., see below
go vet ./cmd/... ./internal/... ./web/...
```

`npm run verify` is the important one for anything touching parsing, elevation
maths or editing — it parses every file in `gpx/`, round-trips them through the
writer, and asserts the invariants that used to break silently.

`./...` matches a stray Go package inside `node_modules`; always scope Go
commands to `. ./internal/... ./web/...` (what the Makefile does).

### Common Commands
```bash
npm install         # frontend dependencies (the Go CLI uses urfave/cli)
make                # npm run build + go build → ./overland
make run            # build and serve on :8000
./overland --help # commands
./overland serve --help
npm run dev         # frontend dev server (http://localhost:5173)
```

## Project Structure

The file tree, the embed/build pipeline and the HTTP API live in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Read it before moving anything
between `src/lib/`, `src/components/` and `internal/server/`.

Two rules it is worth repeating here:

- Keep `src/lib/` free of React and DOM globals apart from `gpx.ts`, which
  needs `DOMParser` (the verify harness supplies one via jsdom).
- The backend is standard library plus Chi routing and middleware. Keep other
  dependencies out unless there is a real reason; the binary should remain
  operationally self-contained.

## Conventions

- **Never fabricate elevation.** Interpolated values must be flagged through to
  the UI, never written to a saved GPX as if measured.
- **Gain/loss always goes through `calculateElevationStats`**, which applies a
  noise floor with direction hysteresis. Raw summation over-reports by 10-50%
  on recorded tracks.
- **Slope needs a distance denominator.** Compute it over a window of at least
  a few tens of metres, never between two adjacent points.
- Elevation lookups go through `lib/elevation.ts` so they stay batched and
  routed via the backend proxy.
- **The filename is a track's identity, not its `<name>`.** The library titles
  cards with `fromGpxFilename`, and save writes back to `track.filename`.
  Deriving the filename from `<name>` forks a second file on every save, and
  tracks that share a `<name>` (four of the sample library are "Created Track")
  collapse onto one filename, silently overwriting each other.
- **Opening a local file is not permission to replace a library file.**
  `POST /upload` is create-only and returns 409 when the filename exists;
  explicit saves through `/gpx/{filename}` keep their replace semantics.
- **The frontend must keep working with no backend.** Library, upload and
  elevation calls are all best-effort; a dropped GPX file still parses and
  displays. Do not turn a backend failure into a dead screen.
- **Session map overlays are not GPX edits.** Keep agent-only map tracks and
  markers out of planner/editor documents, dirty state, undo history, saves,
  and downloads.
- **A loopback peer address proves nothing behind a proxy.** Every request then
  arrives from the proxy, so `remoteIsLoopback` is gated by `--behind-proxy`
  and MCP is confined to its own loopback listener rather than a `Host` check.
  Grant privilege from an admin token or a declared origin, never from
  `RemoteAddr` alone.
- **A tool result means the command was accepted, not that the work
  succeeded.** Anything a command starts asynchronously — routing, elevation,
  surface — must report failure through the snapshot (`planner.routeError`,
  `notifications`), because the agent has already been told `ok`.
- **Treat every filename from the network as hostile.** Anything touching the
  library goes through `safeGPXFilename`, which refuses directory components
  and non-`.gpx` names, then through `os.Root` so symlinks cannot escape
  `GPX_DIR`. There is no authentication in front of it.
- **Never assume an untagged surface is sealed.** Edges with no OSM `surface`
  tag are reported as `unknown` and counted towards neither the paved nor the
  unpaved share — rolling them into either invents a number the data does not
  support. Surface is advisory; it is only as good as the tagging.

## External Services

- **Backend**: same origin as the frontend in the built binary; `:8000` behind
  the Vite dev proxy. `VITE_API_BASE` overrides it for a remote backend.
- **Elevation**: proxied via the backend, which picks the source — terrain-RGB
  tiles with `-elevation-tiles` (~30 m, caches to disk, works offline), an
  opentopodata-style service when `ELEVATION_HOST` is set, or the public
  Open-Meteo API (Copernicus 90 m) when tiles are disabled. All three are
  normalised to `{"results":[{"elevation":…}]}`, so the frontend cannot tell
  them apart, and a point with no data comes back `null` — never `0`, which
  reads as sea level.
  `VITE_ELEVATION_API` adds an optional direct-from-browser fallback and is
  empty by default.
- **Routing**: Valhalla at `valhalla1.openstreetmap.de`, `motorcycle` costing,
  falling back to `auto`/`bicycle` then OSRM if unsupported. Both public hosts
  are operated by FOSSGIS, so every request goes through the shared
  `fetchFossgis` one-request-per-second queue.
- **Surface**: the same Valhalla instance's `/trace_attributes`, which rejects
  any path over 200 km — `lib/surface.ts` chunks around that limit, and the
  verify harness asserts the chunking so it cannot regress silently. Chunks are
  sent sequentially through the same FOSSGIS queue as route requests.
- **Places**: Nominatim, with a one-request-per-second queue and session cache.
  `NOMINATIM_URL` changes the provider at runtime through `/config`.
  **POIs**: Overpass.
- **Fuel prices (Spain)**: `energia.serviciosmin.gob.es`
  `/ServiciosRestCarburantes/PreciosCarburantes/EstacionesTerrestres/` — note
  `Precios` plural, the singular 404s. CORS is open, so no proxy. The feed is
  Spanish-formatted: decimal commas in prices *and* coordinates, and an empty
  price means "not sold", never zero. `lib/poi.ts` prefers it over Overpass
  for fuel inside Spain and falls back to Overpass whenever it has nothing.
- **Tiles**: OpenFreeMap vectors, OpenStreetMap thumbnails/WebGL fallback,
  OpenTopoMap, CyclOSM, Esri imagery/relief/hillshade. The OSM fallback must use
  the exact `https://tile.openstreetmap.org/{z}/{x}/{y}.png` policy URL, with no
  `{s}` subdomains.
