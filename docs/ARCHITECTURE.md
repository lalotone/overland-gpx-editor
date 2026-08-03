# Architecture

A Go server holds the whole app. `web/embed.go` embeds the Vite build with
`go:embed`, so one binary serves the React bundle and the API from the same
origin, and there is nothing to deploy alongside it.

---

## The single binary

```
npm run build   →  web/dist/{index.html,assets/*}
go build        →  //go:embed all:dist  →  ./gpx-editor
```

Build order matters: Go embeds whatever is in `web/dist` at compile time.
`make` runs both in sequence; `go build` on its own produces a working
API-only binary that logs that it has no UI.

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
    ├── fuel.ts                 Spanish official fuel prices
    ├── routing.ts              Valhalla costing and fallbacks
    ├── surface.ts              Per-segment surface via trace_attributes
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

---

## Backend

**No third-party dependencies.** Routing is `http.ServeMux` with Go 1.22 method
patterns (`GET /gpx/{filename}`), and `go.mod` has an empty require block.
Keep it that way unless there is a real reason — the point of the Go rewrite is
one dependency-free binary.

**`files.go`** is a track library over a directory. Every filename arriving from
the network goes through `safeGPXPath`, which requires a bare `*.gpx` with no
directory component; a name such as `../../etc/passwd` is refused with a 400
before it reaches the filesystem. Writes go to a temp file and are renamed into
place, so an interrupted save cannot leave a truncated track behind.

**`elevation.go`** proxies to one of two DEM providers and normalises both to
the same response shape, so the frontend cannot tell them apart:

| Provider | When | Data |
| --- | --- | --- |
| Open-Meteo | default | Copernicus DEM GLO-90 |
| opentopodata | `-elevation-host` set | whatever you self-host |

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
| `POST /upload` | Multipart upload, field `file` |
| `DELETE /gpx/{name}` | Delete a track |
| `GET /elevation?lat=&lon=&dataset=` | Single-point DEM lookup |
| `POST /elevation/batch` | `{locations: "lat,lon\|lat,lon…", dataset}` — chunked and stitched |
| `GET /*` | The React app; unknown paths fall through to it |

Errors come back as `{"detail": "…"}` with a matching status. CORS is open and
there is no authentication of any kind.

---

## Frontend

`src/lib/` holds the domain logic and stays free of React and DOM globals, so
the verify harness can exercise it under Node. The route maths lives here
rather than in Go deliberately: the UI needs it synchronously while dragging a
selection across the elevation profile, and a round trip per interaction would
be visible.

`App.tsx` owns screens and state; everything under `components/` is
presentational.

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
asked for, the dataset allowlist, and both provider dialects.

There is no browser test setup. UI changes need looking at by hand.

> [!TIP]
> `go test ./...` also matches a stray Go package inside `node_modules`. Use
> `./internal/...`, or just `make test`.
