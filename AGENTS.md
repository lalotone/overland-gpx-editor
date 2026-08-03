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
go vet ./internal/... .
```

`npm run verify` is the important one for anything touching parsing, elevation
maths or editing — it parses every file in `gpx/`, round-trips them through the
writer, and asserts the invariants that used to break silently.

`./...` matches a stray Go package inside `node_modules`; always scope Go
commands to `. ./internal/... ./web/...` (what the Makefile does).

### Common Commands
```bash
npm install         # frontend dependencies (Go has none)
make                # npm run build + go build → ./gpx-editor
make run            # build and serve on :8000
./gpx-editor -h     # flags
npm run dev         # frontend dev server (http://localhost:5173)
```

## Project Structure

The file tree, the embed/build pipeline and the HTTP API live in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Read it before moving anything
between `src/lib/`, `src/components/` and `internal/server/`.

Two rules it is worth repeating here:

- Keep `src/lib/` free of React and DOM globals apart from `gpx.ts`, which
  needs `DOMParser` (the verify harness supplies one via jsdom).
- The backend is standard library only. Keep it that way unless there is a
  real reason — the point of the Go rewrite is one dependency-free binary.

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
- **The frontend must keep working with no backend.** Library, upload and
  elevation calls are all best-effort; a dropped GPX file still parses and
  displays. Do not turn a backend failure into a dead screen.
- **Treat every filename from the network as hostile.** Anything touching the
  library goes through `safeGPXPath`, which refuses directory components and
  non-`.gpx` names. There is no authentication in front of it.
- **Never assume an untagged surface is sealed.** Edges with no OSM `surface`
  tag are reported as `unknown` and counted towards neither the paved nor the
  unpaved share — rolling them into either invents a number the data does not
  support. Surface is advisory; it is only as good as the tagging.

## External Services

- **Backend**: same origin as the frontend in the built binary; `:8000` behind
  the Vite dev proxy. `VITE_API_BASE` overrides it for a remote backend.
- **Elevation**: proxied via the backend, which picks the source — the public
  Open-Meteo API (Copernicus 90 m, no setup) by default, or an
  opentopodata-style service when `ELEVATION_HOST` is set, which is the better
  data. Both are normalised to `{"results":[{"elevation":…}]}`, so the frontend
  cannot tell them apart. `VITE_ELEVATION_API` adds an optional direct-from-
  browser fallback and is empty by default.
- **Routing**: Valhalla at `valhalla1.openstreetmap.de`, `motorcycle` costing,
  falling back to `auto`/`bicycle` then OSRM if unsupported.
- **Surface**: the same Valhalla instance's `/trace_attributes`, which rejects
  any path over 200 km — `lib/surface.ts` chunks around that limit, and the
  verify harness asserts the chunking so it cannot regress silently.
- **Places**: Nominatim. **POIs**: Overpass.
- **Fuel prices (Spain)**: `sedeaplicaciones.minetur.gob.es`
  `/ServiciosRESTCarburantes/PreciosCarburantes/EstacionesTerrestres/` — note
  `Precios` plural, the singular 404s. CORS is open, so no proxy. The feed is
  Spanish-formatted: decimal commas in prices *and* coordinates, and an empty
  price means "not sold", never zero. `lib/poi.ts` prefers it over Overpass
  for fuel inside Spain and falls back to Overpass whenever it has nothing.
- **Tiles**: OpenStreetMap, OpenTopoMap, CyclOSM, Esri imagery/relief/hillshade.
