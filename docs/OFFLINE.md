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
| Broom routing data | Local OSM extract, DEM, graph and profile metrics | Explicit region preparation; automatic migration of the active region after an engine upgrade |
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
is opened and warmed for Road, Dirt, Trail and Enduro, with both default and
permit access policies, before it becomes active,
so interactive route requests never trigger graph building or profile
customization.

Planner and Explore use Broom's region suggestions, prioritizing local extracts
containing the viewport centre and explicitly marking partial coverage.
The region catalogue is cached through the backend;
viewport suggestions fetch no PBFs or DEMs and start no preparation jobs. Open the
compact **Offline routing** map pill to accept a download and reveal progress.
The selected generation and explicit pins survive pruning. Protected pin and
prune API operations expose Broom's application-level cache policy.
The last active managed region is persisted and reopened on restart. Older
caches without that selection reopen the newest installed region.

Opening the download panel asks Broom for an acquisition plan without downloading
PBFs or terrain. Exact terrain counts require a local PBF; unknown sizes remain
unknown until it is available. Plans report cached and missing tiles and remaining
source transfer bytes when known, excluding graph and temporary build space.

Progress reports road data, terrain selection, terrain tiles, graph building and
profile preparation separately. Broom supplies aggregate tile totals, downloaded
and reused counts, retries, elapsed stage time and activity updates. Transfer
percentages are per file; diagnostics don't replace current work. Only an open
graph is reported as ready.

`GET /offline/routing/regions` exposes the provider's region hierarchy and bounds
with installed/in-use flags. It starts no preparation. Without the full provider
catalogue, installed generations remain browsable from local metadata. City
map packs still use a covering regional routing extract, not a city-specific
routing graph.

Vector pack preparation clamps tile requests to each source's declared native
maximum zoom. Higher requested display zooms reuse native parent tiles for
overzooming instead of requesting nonexistent tiles from the provider.

Explicit `regional: true` bbox packs use a bounded 100,000-resource budget and
128-resource batches, instead of the ordinary 10,000-resource trip-pack limit.
Two vector workers share the existing provider limiter. Their larger manifests
have reserved control storage and checkpoint every batch (or five seconds),
with terminal states always flushed. One manifest owns all cache pins; terrain
pins are restored before quota eviction at startup. Quota shortages fail admission
rather than silently replacing previously downloaded packs.

Regional elevation work allows up to 16,384 tiles / 2 GiB, still subject to the
configured terrain quota and existing pack reservations. Broad POI searches keep
their provider bounds: `unavailable` reports unsupported resources and a terminal
`provider_limits` result distinguishes those from a network failure. There is no
tiled Overpass sweep. Supported map/elevation work can finish independently.

Broom 0.5 selects sparse terrain tiles intersecting retained highway/ferry
geometry. A real Catalonia plan and rebuild selected 55 tiles, versus 180 with
0.4's all-node rectangle. Retained distant roads and ferries still require terrain;
the extract polygon is not used to discard their elevations.

With Broom 0.6, build pipeline 3 corrects baked road-access reachability counts.
At startup, an incompatible active managed generation is opened directly when
still readable and warmed with the current profiles, then rebuilt in the
background. Selecting another incompatible installed region through ordinary
preparation also triggers migration. Source acquisition uses Broom's managed
cache and revalidation; missing sources can be downloaded. A corrupt catalogue
does not trigger automatic acquisition.

`upgradePending` in the status identifies a still-active older graph;
`job.upgrading` identifies migration progress. Cancellation or failure leaves
that graph usable, with a **Resume routing update** action. Cache-only startup
keeps it open without upstream requests, and switching back online resumes the
update. Directly opened older graphs retain their old reachability counts until
rebuilt. Old generations and source tiles remain until explicitly pruned.
Entering `cache-only` cancels active acquisition and prevents new downloads, but
installed graphs remain routable. A custom `--routing-graph` is opened directly
and is not owned or pruned by the managed routing cache.

### Access permits

The planner's **Restricted access** checkbox sends `accessPermit: true`
to `/routing/broom/route`. It selects a separately warmed metric for the chosen
riding profile. The default is false. The permit bypasses way/node access tags
and the affirmative motor-permission requirement on supported paths, and allows
gates, lift/swing gates and chains. One-way and turn restrictions, solid barriers
and `smoothness=impassable` remain effective. Enduro retains its supported road
classes (including paths and bridleways, but not footways or steps).

This is a route-wide rider declaration, not a geographically scoped permit.
The UI resets it when clearing/resetting the planner and
exposes its effective value in the MCP snapshot. Custom uploaded profiles own
their access policy and reject the override. Broom's endpoint snapping still
uses baked motor reachability counts: a newly allowed path can be traversed
without being eligible as an endpoint. If snapping fails, place route controls
on the connected road network on either side of the restricted section.

## Session BRF profiles

Once a routing graph is ready, **Upload session BRF** compiles and warms a custom
profile for that graph. The BRF is kept in memory, never in the track library or
browser storage. Its derived metrics use a temporary directory. Removing the
profile or closing the browser session releases it; abandoned sessions expire
after two hours and all sessions are released on server shutdown. A region switch
requires uploading again. Uploads are limited to 128 KiB and eight live profiles
per server. This does not change the built-in profiles or saved GPX contents.

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
