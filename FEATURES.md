# GPX Editor — Features

A route planner and track editor for offroad, overlanding and motorbike use.

---

## Terrain and topography

- **Base maps** — OpenStreetMap, OpenTopoMap (contour lines + relief), CyclOSM
  (renders track surface and grade clearly), Esri satellite imagery, Esri
  shaded relief.
- **Hillshade overlay** with adjustable strength, drawn in its own map pane
  above the base tiles and below the track. Works over satellite, where
  imagery alone flattens gullies and ridgelines out.
- **Live ground elevation under the cursor** — hover anywhere and the readout
  shows the terrain altitude at that point, sampled from the DEM and cached.
- **Track colouring by gradient or by altitude.** Altitude mode uses a
  hypsometric ramp (valley green → sand → ochre → rock) scaled to the track's
  own range, so the route itself reads as topography.
- Layer, relief and colour-mode choices persist across sessions.

## Track statistics

- Distance, min/max altitude, gain/loss, point count.
- Gain and loss use a **noise floor with direction hysteresis**: jitter around
  a level contributes nothing, while a genuine steady climb is counted in
  full. Elevation is smoothed over a 60 m *distance* window first, so the
  numbers do not depend on how densely the source device recorded.
- **Moving time and average moving speed** from recorded timestamps, when the
  file has them.
- **Longest gap without fuel** once the fuel layer is loaded.

## Elevation profile

- Distance-sampled bars (not index-sampled), so slope percentages always have
  a meaningful denominator.
- Hover for altitude, distance, delta and gradient; click to fly the map there.
- Legend switches between the gradient scale and the altitude ramp.
- Interpolated elevation is labelled as such.

## Editing

Applies to any loaded track, with undo:

- **Select range** on the profile, then **Crop** to it or **Split** there.
- **Stages** — cut a long route into roughly equal daily distances.
- **Reverse**.
- **Simplify** to a point budget (Douglas-Peucker), for GPS units that cap
  track points and silently truncate beyond it.
- **Smooth ele** — bake noise filtering into the stored elevation.
- **Refetch ele** — re-read every point's altitude from the terrain model,
  for tracks with missing or garbage elevation.

## Route planning

- Click waypoints on the map, drag to adjust, Ctrl+Z to undo, reverse direction.
- **Motorbike routing profiles** via Valhalla `motorcycle` costing:
  - **Road** — sealed roads
  - **Dirt** — prefers unsealed roads and forest tracks over tarmac
  - **Trail** — maximum offroad, narrow tracks and paths where legal
  Falls back to `auto`/`bicycle`, then OSRM, if the server lacks motorcycle
  costing — and says so, because travel time then models a different vehicle.
- Requests are debounced and cancelled, so a slow earlier response can never
  overwrite a newer route.
- Elevation is fetched for **every** routed point in batched, concurrent
  requests. Only past ~6000 points is it sampled and interpolated, and that
  is reported rather than silently written to file.
- Place search via Nominatim.

## Surface

- **What the route is actually made of**, read per segment from Valhalla
  `trace_attributes` while planning: sealed road, gravel/compacted, dirt
  track, path/rough.
- **Distance and share per surface**, weighted by segment length rather than
  segment count — a route is a few long road segments and many short twisty
  dirt ones, so counting segments reports the opposite of the truth.
- **Headline "% unpaved"**, plus a legend keying each colour to its surface.
- **Colour the route by surface** on the map, alongside gradient and altitude.
- Untagged edges are reported as **Unknown** and counted towards neither side,
  never quietly assumed sealed. Surface is only as good as the OSM tagging.
- Long routes are cut into chunks under the server's 200 km trace limit and
  stitched. A trace failure leaves the route and its elevation untouched.

## POIs — fuel, drinking water, campsites

Loaded on demand from Overpass, with two ways to ask:

- **Around a loaded track** — a corridor along the route, on the view screen.
- **In the current map view** — while planning, where there may be no route
  yet to search around. The buttons search whatever the map is showing at the
  moment you press them, and a **Search here** button re-runs the active
  layers after you pan or zoom somewhere else.

Views larger than 250 km across are refused with a prompt to zoom in: Overpass
caps a reply at 400 results, and 400 points spread over half a country reads
as "nothing here" in the gaps where really we just stopped counting.

### Fuel prices in Spain

Inside Spain the fuel layer comes from the **Ministerio de Industria** open
data instead of OSM — the authoritative list of every filling station, with
**the price of each fuel it sells**, updated through the day. Popups show
Gasolina 95/98 and Gasóleo A/Premium in €/L, plus address, opening hours and
the publication timestamp, so a stale price is never read as current.

**Stations are coloured by price** — green for the cheapest in view through to
red for the dearest, with a legend on the map. The ranking is by position
among the stations on screen rather than by absolute price, so one motorway
station at 2.40 € cannot flatten the scale and make every ordinary pump look
like a bargain. Which fuel the colours rank on is switchable (petrol and
diesel disagree about who is cheapest), and stations that do not sell it stay
neutral rather than being ranked against a fuel they do not stock. Where
there is nothing to compare — a single station, or all of them at the same
price — the colouring stays neutral instead of inventing a bargain.

The service allows cross-origin requests, so this runs from the browser with
no proxy. It cannot filter by bounding box — only by province — so the
national list (~11,500 stations, ~12 MB) comes down once per session, on
first use of the fuel layer, and is filtered locally. Anywhere the official
list has nothing to say, including outside Spain or if the service is down,
the OSM fuel layer answers as before.

## Interface

- **Intro animation** — a topographic map drawing itself: contour rings in the
  app's own hypsometric ramp, a route threading the passes between three
  summits, an elevation silhouette rising along the bottom. Entirely generated
  geometry, so it works with no network. Click or press Enter to skip, and it
  is skipped outright under `prefers-reduced-motion`.
- **Library cards** — each saved track shows its route drawn over a small
  map of the ground it crosses, plus an elevation sparkline, distance and
  gain/loss, and a REC badge for recorded tracks. The thumbnail follows the
  base map you last chose, and uses the same Web Mercator projection as the
  tiles, so the line sits on the roads and valleys it actually follows.
  Filter box appears past four tracks.
- **Full map while planning** — a toggle under the zoom control hides the
  sidebar and header so the map fills the window. The map's own overlays
  (terrain, POI search, cursor readout) stay, since those are map tools rather
  than interface. Escape brings the panels back.
- **Collapsible terrain panel** — a pill in the map corner that expands to the
  layer/relief/colour controls, so it stops covering the terrain you're reading.
- **Editing tools behind a Tools toggle**, grouped by what they do; the POI
  layers and undo stay on the always-visible strip.

## File handling

- **Parsing** via DOMParser: tolerant of attribute order, quoting style,
  namespace prefixes and self-closing tags. Reads `<trk>` (concatenating
  segments), `<rte>` route files, and file-level `<wpt>` waypoints.
- **Writing** GPX 1.1 with proper namespace declarations, XML escaping, and
  preservation of per-point elevation, timestamps and waypoints.
- Upload by drag-and-drop or picker; save to and load from the backend library.

## Backend (Go, `internal/server`)

- Ships as a **single binary**: the React build is embedded with `go:embed`
  and served from the same origin as the API, so deploying is one file plus a
  directory of tracks.
- GPX library: list, read, write, upload, delete — all filename-sanitised
  against path traversal. Saves are atomic, so an interrupted write cannot
  leave a truncated track in the library.
- `POST /elevation/batch` chunks large lookups to the upstream DEM limit and
  stitches results back in order.
- Elevation works with no setup: the public Open-Meteo API is the default
  source, a self-hosted opentopodata service takes over when `-elevation-host`
  is set, and `-elevation-tiles` reads ~30 m terrain-RGB rasters directly.
  All three are normalised to one response shape.
- **Offline elevation**: with `-elevation-tile-cache`, fetched terrain tiles
  are kept on disk, so a corridor planned at home still profiles in the field
  with no network.
- **Tiles download ahead of use.** While planning, the terrain covering the
  visible map is fetched in the background with a progress readout; pan
  somewhere else and it follows. Areas too large to be worth caching are
  reported rather than downloaded.
- No third-party Go dependencies — standard library only.

---

## Not done yet

- **Surface on a loaded track** — the breakdown is read while planning; an
  imported GPX would need map-matching before it could be classified.
- **`tracktype`/`smoothness`** — how rough the dirt is, not just that it is
  dirt. Valhalla's `surface` does not carry it; needs an Overpass tag join.
- **Offline tile caching** along a route corridor.
- **Access warnings** — `access=private`, gates, seasonal closures.
- **Mobile layout** — the creation screen is still desktop-shaped.
- **Track joining in the UI** (`joinTracks` exists in `lib/edit.ts`).
