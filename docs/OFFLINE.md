# Offline Cache

Offline mode assumes the browser can still reach the `overland serve` process
and its embedded frontend, but that the Go process cannot reach public
providers. It is not a service worker. Local routing uses a separately managed
Broom graph prepared before going offline.

## Storage

The generic response cache defaults to
`$XDG_CACHE_HOME/overland/responses` (`~/.cache/overland/responses`), with a
1 GiB and 100,000-entry ceiling. An explicitly empty `OFFLINE_CACHE_DIR`
disables new persistent response storage while retaining bounded pass-through
APIs. Terrarium elevation keeps its existing, separate
`$XDG_CACHE_HOME/overland/tiles` tree so upgrades do not move or invalidate
already downloaded DEM tiles. `ELEVATION_TILE_CACHE_MAX_BYTES` bounds that
separate tree to 1 GiB by default; oldest disk tiles are evicted first.

Response filenames are hashes. Search text and POI bounds do
not appear in paths or request logs. Cache directories use mode `0700` and files
use `0600`; writes use temporary files followed by rename. Bodies, sidecars and
pack manifests count towards the quota. Expired entries are removed first,
then least-recently-used unpinned entries. A pack cannot extend a provider's
retention ceiling.

Place and POI caches reveal location history. Set `OFFLINE_CACHE_DIR=`
to opt out of persistence, use the management API to clear an individual
scope, or remove the cache directory while the server is stopped.

## Modes

`auto` serves fresh hits, revalidates stale entries and uses provider-permitted
stale data after retryable failures. `cache-only` makes no outbound request:
memory and disk hits work, and a miss returns `offline_cache_miss` immediately.
This gate includes API elevation, Terrarium misses, background prefetch and pack
workers. The frontend does not bypass an advertised cache-only backend through
its standalone direct-provider fallbacks.

When the server starts in `auto`, the **Work offline** control changes both the
browser's runtime service policy and the server's transport gate. Entering
`cache-only` cancels the active network generation before the control reports
success; new provider and Terrarium requests cannot start, while cache reads
continue. **Go online** creates a new network generation. A server deliberately
started with `--offline-mode cache-only` does not advertise this control and
rejects attempts to switch online, so a browser cannot weaken operator policy.
Server-rendered HTML carries the initial mode, so a cache-only page also starts
closed and remains closed if its later `/config` request fails. Standalone
frontend builds with no backend configured have no such marker and retain their
best-effort direct mode. Setting `VITE_API_BASE` makes that remote backend
authoritative: the bundle starts closed and does not enable direct providers
unless backend config explicitly permits normal `auto` operation.

## Provider Policy

Reviewed 28 August 2026. Provider terms can change; review the linked policies
before changing an adapter or release default.

| Resource | Passive persistent cache | Pack/prefetch |
| --- | --- | --- |
| OSM standard raster | Viewed tiles only; honor headers, seven-day fallback when unusable, no expired replay | Prohibited |
| OpenTopoMap | Viewed tiles with attribution; dated stale replay | Prohibited without operator permission |
| CyclOSM | Viewed tiles, at most 72 hours, no expired replay | Prohibited |
| Esri live imagery/relief/hillshade | Disabled; remains browser-direct | Prohibited |
| Public OpenFreeMap live service | Enabled by default; honor upstream cache headers and attribution | Bounded trip-pack prefetch enabled by default |
| Configured compatible map source | Set with `OPENFREEMAP_URL` | Controlled by `OPENFREEMAP_ALLOW_BULK`, which defaults to `true` |
| Terrarium elevation | Existing immutable tile cache | Bounded viewport/corridor prefetch |
| Fuel snapshot | Explicit open-data snapshot, dated by source and cache | One bounded snapshot |
| Nominatim | Exact user searches, one server-wide request/second | No autocomplete, grid or area sweep |
| Overpass | Exact bounded user POI query | No tiled sweeps or harvesting |
| Broom routing data | Local OSM extract, DEM, graph and profile metrics | Explicit region preparation only |
| API elevation | Exact source/dataset coordinates | Bounded requests subject to provider licence/quota |

Policy sources: [OSMF tiles](https://operations.osmfoundation.org/policies/tiles/),
[OpenTopoMap](https://opentopomap.org/about),
[CyclOSM](https://www.cyclosm.org/),
[Esri terms](https://www.esri.com/en-us/legal/terms/full-master-agreement),
[OpenFreeMap public-instance policy](https://openfreemap.org/) and
[terms](https://openfreemap.org/tos/),
[Nominatim](https://operations.osmfoundation.org/policies/nominatim/),
[Overpass](https://dev.overpass-api.de/overpass-doc/en/preface/commons.html),
[Open-Meteo](https://open-meteo.com/en/terms), and the
[Spanish fuel catalogue](https://datos.gob.es/es/catalogo/e05068001-precio-de-carburantes-en-las-gasolineras-espanolas).

## Routing Data

Broom routing data lives under `$XDG_CACHE_HOME/overland/routing`, outside the
generic response-cache quota. `POST /offline/routing/prepare` explicitly
downloads and builds one region; status and progress are available from
`GET /offline/routing`, and cancellation is cooperative. A completed generation
is opened and warmed for all three motorcycle profiles before it becomes active,
so interactive route requests never trigger graph building or profile
customization.

Planner and Explore suggest the smallest downloadable extract whose polygon
covers the complete viewport. A coastal view that would otherwise select a
continent instead offers a local centre-containing extract, explicitly marked
as partial coverage. The region catalogue is cached through the backend;
viewport suggestions fetch no PBFs or DEMs and start no preparation jobs. Open the
compact **Offline routing** map pill to accept a download and reveal progress.
The selected generation and explicit pins survive pruning. Protected pin and
prune API operations expose Broom's application-level cache policy.
The last active managed region is persisted and reopened on restart. Older
caches without that selection reopen the newest installed region.

Progress reports road data, terrain tiles, graph building and profile preparation
separately. Transfer percentages are per file; completed terrain tile counts do
not reset between files. Only an open graph is reported as ready.

Broom 0.4 determines terrain acquisition from all PBF node bounds, including
distant relation members. Some extracts therefore acquire substantially more
terrain than their advertised coverage; a live Catalonia preparation required
180 HGT tiles. This is a remaining upstream acquisition limitation, not a retry
loop, and the UI reports the tile count rather than a misleading overall percent.
Entering `cache-only` cancels active acquisition and prevents new downloads, but
installed graphs remain routable. A custom `--routing-graph` is opened directly
and is not owned or pruned by the managed routing cache.

## Trip Packs

Opening or selecting a GPX makes its route the active automatic pack. The
frontend estimates first, then starts preparation without a second user action.
The readiness control reports each resource independently and replaces the
active status when another route is loaded. Recent packs remain pinned; once
the manifest limit is reached, starting a new automatic pack releases the oldest
completed automatic one. Editing an already loaded track does not restart the
job.

Estimates perform no provider traffic. They report resource counts, estimated
and reusable bytes, remaining quota, and every blocked provider. Long and
antimeridian routes are split into bounded corridor POI searches. Jobs have hard
count, byte, zoom, coordinate and worker limits, can be cancelled through the
API, and persist progress after each resource.

A completed pack may contain Terrarium corridor tiles, one fuel snapshot,
bounded trip POIs, selected exact cached data and bounded OpenFreeMap coverage.
It never generates speculative routes, sweeps Nominatim or downloads public
raster basemaps. Cancellation or process restart leaves a pack incomplete;
completed shared cache entries remain valid. A failed provider marks its own
resource unavailable while preparation continues for unrelated resources.

Explore creates the same kind of bounded pack from a drawn rectangle. Estimates
run automatically after the bounds or options settle and remain traffic-free.
Completed area summaries expose only their validated bbox, never the manifest's
route, cache keys or request data. The frontend renders those bboxes as light
coverage rectangles, restores them after reload, and shows them again whenever
the download tool opens. The dedicated manager can hide coverage, inspect all
area jobs, cancel active work and delete completed packs with confirmation.

## Operational Statistics

`overland serve` writes one aligned, human-readable aggregate table per minute
by default. Set `--stats-log-interval` / `STATS_LOG_INTERVAL` to another duration,
or `0` to disable it. The rows cover cache entries and bytes, get
and response-state counts, outbound status classes and timing, current/peak
queue pressure, Terrarium memory/disk usage and pack-state counts.

The summary never includes provider URLs, request paths or queries, search text,
coordinates, response bodies, cache keys, pack IDs/names, credentials or GPX
content. Counters are cumulative for the process; inventory and in-flight values
describe the instant at which the group is written.

## Management Security

Browser mutations require a trusted same-origin request and the
`X-GPX-Editor` header. Without configuration, management is limited to an
actual loopback peer using a `localhost` or loopback-IP origin; matching an
arbitrary `Host` is not enough. Every non-loopback frontend, including a public
same-origin deployment behind a reverse proxy, must set `TRUSTED_UI_ORIGIN`.
Requests without an `Origin` are accepted only from a loopback peer or with
`Authorization: Bearer <OFFLINE_ADMIN_TOKEN>`. These checks reduce CSRF, DNS
rebinding and relay abuse; they do not authenticate the rest of a public
deployment. Keep the server on loopback or put it behind an authenticated
reverse proxy.

This boundary covers `PUT /offline/mode`, pack creation/cancellation/deletion
and cache clearing. Read-only status and public pack summaries expose aggregate
state only.

Traffic-generating read endpoints use a related relay guard. Loopback clients
may use loopback origins; every remote browser host must be named explicitly in
`ALLOWED_ORIGINS` (a trusted UI origin is included automatically). Merely making
`Origin` equal an attacker-controlled `Host` is not accepted.
