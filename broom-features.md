# Broom features Overland needs

Gaps found while building offline region downloads (web and Android). Each
entry says what Overland does today without it.

## 1. Whole-preparation progress

`EnsureRegion` reports progress per phase (`region`, `index`, `pbf`,
`planning`, `elevation`, `build`, `warmup`), and within `build` per sub-stage
(`pbf-node-index`, `topology-*`, `partition`, …). Each has its own
`Done`/`Total` — bytes for downloads, counts elsewhere — and several build
stages (`partition`, `components`, `restrictions`, `write`,
`sample-elevation`) run through `runStage` with no measure at all. The phase
order is not documented as a contract.

A client therefore cannot show one honest percentage: any bar built from these
numbers restarts at every stage, and in the terrain phase at every file.

**Wanted:** an overall progress estimate for the whole preparation (weighted
by expected cost, monotonic), or at least the stage index and stage count, plus
a documented phase order.

**Today:** Overland shows four steps (road data, terrain, routing graph,
riding profiles) with the current one marked, describes the current activity in
plain words, and draws a bar only for measured activities (road-data bytes,
terrain tiles by `ItemsDone/ItemsTotal`, counted build stages). Unmeasured
stages show a spinner and elapsed time. See `routingJobView` in
`src/lib/offlineRegions.ts`.

## 2. Measured progress for long build stages

`partition` in particular can run for minutes on a country with no progress
beyond liveness ticks.

**Wanted:** `Done`/`Total` (or an estimate) for `partition`, `components`,
`restrictions`, `sample-elevation` and `write`.

## 3. Deleting a downloaded region

`Prune` keeps every selected or pinned generation, and there is no call to
remove a region. Users can remove map, terrain and POI packs, but not the
routing data of a region they no longer need.

**Wanted:** `RemoveRegion(ctx, regionID)` that deletes the region's
generations, metrics and (optionally) its sources, refusing the region that is
currently open.

**Today:** routing regions can be switched but not removed from the UI.

## 4. Routing across more than one region

The manager keeps one graph open, and routes use only that graph. So a
country must be downloaded as one extract: Spain as nineteen regional graphs
could not route from Aragón into Navarra without switching regions.

**Wanted:** either routing across adjacent open graphs, or building one graph
from several extracts (a trip across Spain and Portugal, or a few neighbouring
regions instead of a whole country).

**Today:** routing is always the single catalogue extract that covers the
selected area; maps, terrain and POIs are what download region by region.

## 5. Graphs for a custom area

Most countries (239 of the 257 in Geofabrik's index) are a single extract, so
there is no smaller routing download than the whole country.

**Wanted:** building a graph clipped to a bounding box or polygon inside a
catalogue extract, so a rider can prepare routing for the area of a trip.

**Today:** those countries' maps split into a grid of areas, but routing still
needs the whole country extract.
