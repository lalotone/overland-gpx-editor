/*
 * Verification harness for the pure logic — parsing, elevation maths and
 * editing — run against whatever GPX files you keep in ./gpx.
 *
 *   npm run verify
 */

import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { JSDOM } from 'jsdom'
// Type-only, so it is erased before the parser needs a DOM.
import type { Track } from '../src/lib/types'

// The parser targets the browser DOM; give it one.
const dom = new JSDOM()
globalThis.DOMParser = dom.window.DOMParser

const { parseGPX, buildGPX, toGpxFilename, fromGpxFilename } = await import('../src/lib/gpx')
const {
  calculateDistance,
  calculateElevationStats,
  cumulativeDistanceKm,
  smoothElevations,
  calculateTimeStats,
  slopePercent,
  longestGapKm,
} = await import('../src/lib/geo')
const { simplifyToMaxPoints, trimTrack, reverseTrack, splitIntoStages } = await import('../src/lib/edit')
const { chunkShape, classifySurface, summarizeSurface } = await import('../src/lib/surface')
const { boundingBoxSpanKm, boundsAround, MAX_SEARCH_SPAN_KM } = await import('../src/lib/poi')
const {
  parseFuelStations,
  intersectsSpain,
  priceBands,
  fuelBandColors,
  availableFuels,
  FUEL_PRICE_BANDS,
  FUEL_NO_PRICE_COLOR,
  FUEL_PRICE_ENDPOINT,
} = await import('../src/lib/fuel')
const { createRateLimitedFetch } = await import('../src/lib/rateLimit')
const { resolveMapStyleResources } = await import('../src/lib/mapStyle')
const { NOMINATIM_REQUEST_INTERVAL_MS, searchPlaces } = await import('../src/lib/geocoding')
const { calculateRoute, FOSSGIS_REQUEST_INTERVAL_MS } = await import('../src/lib/routing')
const {
  getBaseLayerFrom,
  getThumbnailLayer,
  runtimeHillshadeLayer,
  runtimeTerrainLayers,
  SERVICE_ATTRIBUTIONS,
} = await import('../src/lib/terrain')
const {
  elevationPointCacheKey,
  fetchElevationProfile,
  fetchGroundElevation,
} = await import('../src/lib/elevation')
const { clearedRouteDerivedState, routeSequenceIsCurrent } = await import('../src/lib/planner')
const {
  buildAutomaticPackRequest,
  buildPackEstimateRequest,
  bootstrapRuntimeConfig,
  decodePacks,
  decodePackEstimate,
  decodeOfflineStatus,
  decodeRuntimeConfig,
  fetchRuntimeService,
  formatBytes,
  formatCacheContext,
  loadRuntimeConfig,
  normalizePackBounds,
  OfflineCacheMissError,
  packRequestSignature,
  parseCacheMetadata,
  resolveApiUrl,
  responseError,
  selectRuntimeTransport,
  setRuntimeOfflineMode,
  syncPackLayers,
  validPackArea,
} = await import('../src/lib/offline')

let failures = 0
let checks = 0
let skipped = 0

function check(label: string, condition: boolean, detail = '') {
  checks++
  if (!condition) {
    failures++
    console.log(`  FAIL  ${label}${detail ? ` — ${detail}` : ''}`)
  }
}

/*
 * The library is whatever GPX files you keep in ./gpx — it is gitignored, so
 * a fresh clone has none and the file-backed checks skip themselves rather
 * than failing. Fixtures are chosen by shape (longest track, one carrying
 * waypoints) rather than by filename: the harness must not depend on one
 * person's rides, and those filenames are nobody else's business.
 */
const GPX_DIR = join(process.cwd(), 'gpx')
const files = existsSync(GPX_DIR)
  ? readdirSync(GPX_DIR).filter(f => f.toLowerCase().endsWith('.gpx')).sort()
  : []

interface LibraryEntry {
  file: string
  track: Track
}
const library: LibraryEntry[] = []

/** Longest track matching `predicate`, or null when the library has none. */
function fixture(what: string, predicate: (t: Track) => boolean = () => true): LibraryEntry | null {
  const found = library
    .filter(entry => predicate(entry.track))
    .sort((a, b) => b.track.coordinates.length - a.track.coordinates.length)[0]
  if (!found) {
    skipped++
    console.log(`  SKIP  ${what} — no suitable file in ./gpx`)
    return null
  }
  return found
}

console.log(`\nParsing ${files.length} GPX files\n${'='.repeat(78)}`)

for (const file of files) {
  const content = readFileSync(join(GPX_DIR, file), 'utf-8')
  let tracks
  try {
    tracks = parseGPX(content)
  } catch (err) {
    failures++
    console.log(`  FAIL  ${file} threw: ${(err as Error).message}`)
    continue
  }

  check(`${file} yields at least one track`, tracks.length > 0)
  if (tracks.length === 0) continue

  const track = tracks[0]
  library.push({ file, track })
  const cum = cumulativeDistanceKm(track.coordinates)
  const smoothed = smoothElevations(track.elevations, cum)
  const rawStats = calculateElevationStats(track.elevations, 0)
  const stats = calculateElevationStats(smoothed)
  const time = calculateTimeStats(track.coordinates)

  check(`${file} has coordinates`, track.coordinates.length > 1)
  check(
    `${file} elevations array matches coordinates`,
    track.elevations.length === track.coordinates.length,
  )
  check(`${file} distance is finite`, Number.isFinite(calculateDistance(track.coordinates)))
  check(`${file} gain is finite`, Number.isFinite(stats.gain))

  const withEle = track.elevations.filter(e => e !== null).length
  console.log(
    `  ${file.padEnd(40)} ${String(track.coordinates.length).padStart(6)} pts  ` +
      `${calculateDistance(track.coordinates).toFixed(1).padStart(7)} km  ` +
      `ele ${withEle}/${track.coordinates.length}  ` +
      `wpt ${track.waypoints.length}  ` +
      `gain raw ${rawStats.gain.toFixed(0).padStart(5)}m → filtered ${stats.gain.toFixed(0).padStart(5)}m` +
      (time ? `  moving ${(time.movingSeconds / 3600).toFixed(1)}h @ ${time.movingSpeedKmh.toFixed(0)}km/h` : ''),
  )
}

console.log(`\nRound-trip and edit checks\n${'='.repeat(78)}`)

// Round-trip: everything we parse must survive being written back out. Needs
// a recording, so timestamps and elevation are exercised too.
{
  const entry = fixture('round-trip', t => t.coordinates.some(c => c.time))
  if (entry) {
  const original = entry.track
  const rewritten = buildGPX({
    name: original.name,
    coordinates: original.coordinates,
    waypoints: original.waypoints,
    time: original.time,
  })
  const [reparsed] = parseGPX(rewritten)

  check('round-trip keeps point count', reparsed.coordinates.length === original.coordinates.length,
    `${reparsed.coordinates.length} vs ${original.coordinates.length}`)
  check('round-trip keeps timestamps',
    reparsed.coordinates.filter(c => c.time).length === original.coordinates.filter(c => c.time).length)
  check('round-trip keeps elevation',
    reparsed.elevations.filter(e => e !== null).length === original.elevations.filter(e => e !== null).length)
  check('round-trip keeps the name', reparsed.name === original.name)
  check('output declares the GPX namespace', rewritten.includes('xmlns="http://www.topografix.com/GPX/1/1"'))
  console.log(`  round-trip of ${original.coordinates.length} points, ${original.waypoints.length} waypoints: ok`)
  }
}

// XML escaping — an ampersand in a name used to produce an invalid file.
{
  const gpx = buildGPX({
    name: 'Ruta & "Guara" <test>',
    coordinates: [{ lat: 42.1, lon: -0.4, elevation: 500 }, { lat: 42.2, lon: -0.5, elevation: 600 }],
  })
  const [parsed] = parseGPX(gpx)
  check('special characters survive escaping', parsed.name === 'Ruta & "Guara" <test>', parsed.name)
  console.log(`  escaped name round-trips as: ${parsed.name}`)
}

// Waypoints must survive — they used to be dropped entirely.
{
  const entry = fixture('waypoint round-trip', t => t.waypoints.length > 0)
  if (entry) {
    const { track } = entry
    const rewritten = buildGPX({ name: track.name, coordinates: track.coordinates, waypoints: track.waypoints })
    const [reparsed] = parseGPX(rewritten)
    check('waypoints survive export', reparsed.waypoints.length === track.waypoints.length,
      `${reparsed.waypoints.length} of ${track.waypoints.length}`)
    check('waypoint names survive export',
      reparsed.waypoints.every((w, i) => w.name === track.waypoints[i].name))
    console.log(`  ${track.waypoints.length} waypoints round-tripped`)
  }
}

// Places of interest sit away from the line — a monument near the route, not
// a point on it. They must survive a save with their names, and must not be
// mistaken for track points on the way back in.
{
  const gpx = buildGPX({
    name: 'Ride with places',
    coordinates: [
      { lat: 42.0, lon: -0.5, elevation: 400 },
      { lat: 42.1, lon: -0.6, elevation: 500 },
    ],
    waypoints: [
      { lat: 42.4, lon: -0.9, name: 'Ermita de San Juan' },
      { lat: 41.7, lon: -0.2, name: 'Mirador', desc: 'Worth the detour' },
    ],
  })
  const [reparsed] = parseGPX(gpx)

  check('places of interest survive a save', reparsed.waypoints.length === 2,
    `${reparsed.waypoints.length}`)
  check('their names survive', reparsed.waypoints[0]?.name === 'Ermita de San Juan',
    reparsed.waypoints[0]?.name)
  check('a place description survives', reparsed.waypoints[1]?.desc === 'Worth the detour')
  // The whole point: they are off the line, so the route must be unchanged.
  check('places are not folded into the track', reparsed.coordinates.length === 2,
    `${reparsed.coordinates.length} track points`)
  check('places keep their own position',
    reparsed.waypoints[0].lat === 42.4 && reparsed.waypoints[0].lon === -0.9)
  console.log(`  off-route places: ${reparsed.waypoints.map(w => w.name).join(', ')} — track still ${reparsed.coordinates.length} points`)
}

// A <rte>-only file (planner exports) must load.
{
  const routeOnly = `<?xml version="1.0"?>
<gpx version="1.1" creator="test" xmlns="http://www.topografix.com/GPX/1/1">
  <rte><name>Planned</name>
    <rtept lat="42.0" lon="-0.5"><ele>400</ele></rtept>
    <rtept lat="42.1" lon="-0.6"><ele>500</ele></rtept>
  </rte>
</gpx>`
  const tracks = parseGPX(routeOnly)
  check('<rte>-only file loads', tracks.length === 1 && tracks[0].coordinates.length === 2)
  console.log(`  <rte>-only file: ${tracks.length} track, ${tracks[0]?.coordinates.length} points`)
}

// Attribute order, quoting and self-closing tags vary between exporters.
{
  const awkward = `<?xml version="1.0"?>
<gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1">
  <trk><trkseg>
    <trkpt lon='-0.5' lat='42.0'><ele>400</ele></trkpt>
    <trkpt   lat="42.1"    lon="-0.6" />
    <trkpt lat="42.2" lon="-0.7"><ele>600</ele></trkpt>
  </trkseg></trk>
</gpx>`
  const [track] = parseGPX(awkward)
  check('lon-before-lat, single quotes and self-closing trkpt all parse',
    track?.coordinates.length === 3, `got ${track?.coordinates.length}`)
  check('missing <ele> becomes null, not a dropped point', track?.elevations[1] === null)
  console.log(`  awkward-formatting file: ${track?.coordinates.length} points parsed`)
}

// Editing operations. The fixture is size-bounded on purpose: simplifying a
// 15k-point track to a 500-point budget is a 30:1 reduction, which drifts well
// past 2% for reasons that are geometry, not a bug. A day-sized recording is
// what the 2% claim is about.
{
  const entry = fixture('editing operations',
    t => t.coordinates.length > 500 && t.coordinates.length <= 6000)
  if (entry) {
  const { track } = entry
  const before = track.coordinates.length

  const trimmed = trimTrack(track, 100, 200)
  check('trim keeps the requested span', trimmed.coordinates.length === 101, `${trimmed.coordinates.length}`)

  const reversed = reverseTrack(track)
  check('reverse preserves length', reversed.coordinates.length === before)
  check('reverse swaps the endpoints',
    reversed.coordinates[0].lat === track.coordinates[before - 1].lat)
  check('reverse preserves distance',
    Math.abs(calculateDistance(reversed.coordinates) - calculateDistance(track.coordinates)) < 1e-6)

  const simplified = simplifyToMaxPoints(track, 500)
  check('simplify respects the point budget', simplified.coordinates.length <= 500,
    `${simplified.coordinates.length}`)
  const distanceDrift = Math.abs(
    calculateDistance(simplified.coordinates) - calculateDistance(track.coordinates),
  )
  check('simplify preserves distance within 2%',
    distanceDrift / calculateDistance(track.coordinates) < 0.02,
    `drift ${distanceDrift.toFixed(2)} km`)

  const stages = splitIntoStages(track, 40)
  const stageTotal = stages.reduce((sum, s) => sum + calculateDistance(s.coordinates), 0)
  check('stages cover the whole track',
    Math.abs(stageTotal - calculateDistance(track.coordinates)) < 0.5,
    `${stageTotal.toFixed(2)} vs ${calculateDistance(track.coordinates).toFixed(2)}`)

  console.log(
    `  ${before} pts → simplified ${simplified.coordinates.length} pts ` +
      `(distance drift ${distanceDrift.toFixed(3)} km), ${stages.length} stages of ~40 km`,
  )
  }
}

// Noise floor behaviour.
{
  // A dead-flat track with ±1 m of jitter must report no gain at all.
  const jittery = Array.from({ length: 400 }, (_, i) => (i % 2 === 0 ? 500 : 501))
  check('jitter under the noise floor contributes no gain',
    calculateElevationStats(jittery).gain === 0,
    `${calculateElevationStats(jittery).gain}`)
  check('the same jitter inflates an unfiltered sum',
    calculateElevationStats(jittery, 0).gain > 190)

  // A genuine steady climb must be counted in full despite the floor.
  const climb = Array.from({ length: 101 }, (_, i) => 500 + i)
  check('a real 100 m climb is fully counted',
    Math.abs(calculateElevationStats(climb).gain - 100) < 1e-6,
    `${calculateElevationStats(climb).gain}`)

  console.log(
    `  flat-with-jitter: unfiltered ${calculateElevationStats(jittery, 0).gain.toFixed(0)}m → ` +
      `filtered ${calculateElevationStats(jittery).gain.toFixed(0)}m; ` +
      `steady 100m climb → ${calculateElevationStats(climb).gain.toFixed(0)}m`,
  )
}

/* -- Slope and fuel gap ---------------------------------------------- */
{
  check('slope over a real run', Math.abs(slopePercent(50, 500) - 10) < 1e-9)
  check('slope is signed', slopePercent(-50, 500) === -10)
  // The denominator guard is the whole point: a zero-length run between two
  // coincident points must not produce Infinity or NaN on the profile.
  check('a zero-length run yields 0, not Infinity', slopePercent(5, 0) === 0)
  check('a non-finite run yields 0', slopePercent(5, NaN) === 0)

  // Stations at 0, 30 and 100 km along a 150 km line: the longest dry stretch
  // is the 50 km between the last one and the end.
  const line = Array.from({ length: 151 }, (_, i) => ({ lat: 42, lon: -1 + i * 0.012 }))
  const cum = cumulativeDistanceKm(line)
  const stations = [0, 30, 100].map(km => {
    const idx = cum.findIndex(c => c >= km)
    return { lat: line[idx].lat, lon: line[idx].lon }
  })
  // The dry stretch is the 70 km between the 30 and 100 km stations, less the
  // 2 km corridor either side that still counts as reachable.
  const { gapKm, atKm } = longestGapKm(line, cum, stations)
  check('longest fuel gap found', Math.abs(gapKm - 66) < 3, `${gapKm.toFixed(1)} km`)
  check('the gap is reported where it starts', Math.abs(atKm - 32) < 3, `${atKm.toFixed(1)} km`)
  check('no stations means one gap the length of the route',
    Math.abs(longestGapKm(line, cum, []).gapKm - cum[cum.length - 1]) < 1e-9)

  console.log(`  fuel gap: stations at 0/30/100 km of ${cum[cum.length - 1].toFixed(0)} km ` +
    `→ longest dry stretch ${gapKm.toFixed(1)} km from km ${atKm.toFixed(0)}`)
}

check('filename slug strips accents and spaces',
  toGpxFilename('Mañaneo por Guara ') === 'mananeo-por-guara.gpx',
  toGpxFilename('Mañaneo por Guara '))

/* -- Library titles -------------------------------------------------- */

{
  // The library titles cards off the filename, so this is what the user reads.
  const titles: [string, string][] = [
    ['north-loop-via-town.gpx', 'North loop via town'],
    ['summer_gravel_route.gpx', 'Summer gravel route'],
    ['Ridge-Traverse.GPX', 'Ridge Traverse'],
    // Hyphens between digits are dates and times, not slug separators.
    ['2020-01-01_09-30_Wed.gpx', '2020-01-01 09-30 Wed'],
    ['track1.gpx', 'Track1'],
  ]
  for (const [file, want] of titles) {
    check(`title of ${file}`, fromGpxFilename(file) === want, fromGpxFilename(file))
  }

  // Distinct files must stay distinct on screen — the whole point of titling
  // off the filename rather than a <name> four files share.
  const shared = ['export1.gpx', 'export2.gpx', 'export3.gpx', 'export4.gpx']
  check('files sharing a GPX <name> still get distinct titles',
    new Set(shared.map(fromGpxFilename)).size === shared.length)
}

/* -- POI search area ------------------------------------------------ */

{
  // The viewport guard is the thing standing between a zoomed-out map and a
  // country-sized Overpass query that returns a capped, misleading answer.
  const zaragozaView = { south: 41.58, west: -0.98, north: 41.72, east: -0.78 }
  const city = boundingBoxSpanKm(zaragozaView)
  check('a city-sized view is a searchable size',
    Math.max(city.widthKm, city.heightKm) < MAX_SEARCH_SPAN_KM,
    `${city.widthKm.toFixed(0)}x${city.heightKm.toFixed(0)} km`)

  const aragonView = { south: 39.8, west: -2.1, north: 42.9, east: 0.8 }
  const region = boundingBoxSpanKm(aragonView)
  check('a region-sized view is refused',
    Math.max(region.widthKm, region.heightKm) > MAX_SEARCH_SPAN_KM,
    `${region.widthKm.toFixed(0)}x${region.heightKm.toFixed(0)} km`)

  // Longitude degrees shrink towards the poles; ignoring that would let a
  // northern view through at well over the limit.
  const equator = boundingBoxSpanKm({ south: -0.5, west: 0, north: 0.5, east: 1 })
  const arctic = boundingBoxSpanKm({ south: 69.5, west: 0, north: 70.5, east: 1 })
  check('east-west span narrows with latitude', arctic.widthKm < equator.widthKm * 0.4,
    `${arctic.widthKm.toFixed(0)} km vs ${equator.widthKm.toFixed(0)} km`)
  check('north-south span does not vary with latitude',
    Math.abs(arctic.heightKm - equator.heightKm) < 1e-9)
  const antimeridian = boundingBoxSpanKm({ south: -0.5, west: 179, north: 0.5, east: -179 })
  check('area dimensions use the short antimeridian span',
    antimeridian.widthKm > 200 && antimeridian.widthKm < 230,
    `${antimeridian.widthKm.toFixed(0)} km`)

  // boundsAround pads outwards; it must never come back inverted.
  const padded = boundsAround([{ lat: 41.6, lon: -0.9 }, { lat: 41.7, lon: -0.8 }], 5)!
  check('padded bounds contain the points',
    padded.south < 41.6 && padded.north > 41.7 && padded.west < -0.9 && padded.east > -0.8)

  console.log(
    `  search guard: city ${city.widthKm.toFixed(0)}x${city.heightKm.toFixed(0)} km ok, ` +
      `region ${region.widthKm.toFixed(0)}x${region.heightKm.toFixed(0)} km refused ` +
      `(limit ${MAX_SEARCH_SPAN_KM} km)`,
  )
}

/* -- Spanish fuel price feed ---------------------------------------- */

{
  // The feed is Spanish-formatted throughout: a decimal comma read as a
  // thousands separator turns 1,819 EUR/L into 1819, and 41,84 degrees of
  // latitude into a coordinate off the planet.
  const raw = {
    'IDEESS': '1375',
    'Rótulo': 'REPSOL',
    'Dirección': 'CARRETERA N-122 KM. 53,5',
    'Municipio': 'Agón',
    'Horario': 'L-D: 06:00-22:00',
    'Latitud': '41,840056',
    'Longitud (WGS84)': '-1,419444',
    'Precio Gasolina 95 E5': '1,819',
    'Precio Gasolina 98 E5': '1,949',
    'Precio Gasoleo A': '1,929',
    'Precio Gasoleo Premium': '',
    'Precio Hidrogeno': '',
  }
  const [station] = parseFuelStations([raw])

  check('latitude parses through the decimal comma',
    Math.abs(station.lat - 41.840056) < 1e-9, `${station.lat}`)
  check('negative longitude parses through the decimal comma',
    Math.abs(station.lon - -1.419444) < 1e-9, `${station.lon}`)
  check('a price is euros per litre, not thousands',
    station.prices.some(p => Math.abs(p.price - 1.819) < 1e-9),
    JSON.stringify(station.prices))
  check('an address containing a comma is left alone',
    station.address === 'CARRETERA N-122 KM. 53,5', station.address)

  // An empty price means the station does not sell that fuel. Reading it as
  // zero would advertise free diesel and poison any "cheapest nearby" answer.
  check('an unsold fuel is omitted, not zero',
    station.prices.every(p => p.price > 0) && !station.prices.some(p => p.label === 'Gasóleo Premium'),
    JSON.stringify(station.prices))
  check('only the fuels worth showing are kept', station.prices.length === 3,
    `${station.prices.length}`)

  // A station with no usable position must be dropped, not placed at 0,0 off
  // the coast of Africa.
  check('a station with no coordinates is dropped',
    parseFuelStations([{ ...raw, 'Latitud': '' }]).length === 0)

  check('Spain is recognised', intersectsSpain({ south: 41.5, west: -1.0, north: 41.8, east: -0.7 }))
  check('the Canaries are recognised',
    intersectsSpain({ south: 28.0, west: -16.0, north: 28.5, east: -15.5 }))
  check('a view far outside Spain is not',
    !intersectsSpain({ south: 52.3, west: 13.2, north: 52.6, east: 13.6 }))

  console.log(
    `  feed parsing: ${station.brand} ${station.town} ` +
      `${station.lat.toFixed(4)},${station.lon.toFixed(4)} — ` +
      station.prices.map(p => `${p.label} ${p.price.toFixed(3)}`).join(', '),
  )
}

{
  // Cheapest green, dearest red — ranked, so one outlier cannot flatten the
  // scale and leave every ordinary station looking like a bargain.
  const spread = [1.60, 1.70, 1.75, 1.80, 1.85, 1.90, 1.95, 2.00, 2.10, 2.40]
  const bands = priceBands(spread)
  check('the cheapest price lands in the cheapest band', bands[0] === 0, `${bands[0]}`)
  check('the dearest price lands in the dearest band',
    bands[bands.length - 1] === FUEL_PRICE_BANDS.length - 1, `${bands[bands.length - 1]}`)
  check('bands never decrease as price rises',
    bands.every((b, i) => i === 0 || b >= bands[i - 1]), bands.join(','))
  check('every band index is in range',
    bands.every(b => b >= 0 && b < FUEL_PRICE_BANDS.length))

  // One absurd outlier must not drag everything else into "cheap".
  const withOutlier = priceBands([1.60, 1.62, 1.64, 1.66, 9.99])
  check('an outlier does not collapse the scale',
    new Set(withOutlier.slice(0, 4)).size > 1, withOutlier.join(','))

  // Equal prices must look equal, or two identical stations get different
  // colours and the map appears to know something it does not.
  const ties = priceBands([1.70, 1.85, 1.70, 1.90, 1.85])
  check('equal prices share a band', ties[0] === ties[2] && ties[1] === ties[4], ties.join(','))

  const middle = Math.floor((FUEL_PRICE_BANDS.length - 1) / 2)
  check('a lone station is neutral, not the cheapest',
    priceBands([1.80])[0] === middle, `${priceBands([1.80])[0]}`)
  check('an all-equal set is neutral, not all cheapest',
    priceBands([1.8, 1.8, 1.8]).every(b => b === middle))
  check('an empty set bands nothing', priceBands([]).length === 0)

  // Colouring only applies to priced stations; OSM fallback keeps its own.
  const osmPois = [{ id: 'node/1', kind: 'fuel' as const, lat: 41, lon: -1 }]
  check('unpriced OSM stations are left their layer colour',
    fuelBandColors(osmPois, 'Gasolina 95').size === 0)

  const mixed = [
    { id: 'a', kind: 'fuel' as const, lat: 41, lon: -1, prices: { 'Gasolina 95': 1.60 } },
    { id: 'b', kind: 'fuel' as const, lat: 41, lon: -1, prices: { 'Gasolina 95': 2.20 } },
    { id: 'c', kind: 'fuel' as const, lat: 41, lon: -1, prices: { 'Gasóleo A': 1.90 } },
  ]
  const colors = fuelBandColors(mixed, 'Gasolina 95')
  check('the cheapest of the fuel being ranked is green',
    colors.get('a') === FUEL_PRICE_BANDS[0].color, colors.get('a'))
  check('the dearest of the fuel being ranked is red',
    colors.get('b') === FUEL_PRICE_BANDS[FUEL_PRICE_BANDS.length - 1].color, colors.get('b'))
  check('a station not selling that fuel is not ranked against it',
    colors.get('c') === FUEL_NO_PRICE_COLOR, colors.get('c'))
  check('only fuels actually on offer are listed',
    availableFuels(mixed).join(',') === 'Gasolina 95,Gasóleo A', availableFuels(mixed).join(','))

  console.log(
    `  price bands: ${spread.map((p, i) => `${p.toFixed(2)}→${bands[i]}`).join('  ')}`,
  )
}

/* -- Public-service policy ------------------------------------------- */

console.log(`\nPublic-service policy checks\n${'='.repeat(78)}`)

{
  let active = 0
  let maxActive = 0
  const starts: number[] = []
  const limitedFetch = createRateLimitedFetch(25, async () => {
    starts.push(Date.now())
    active++
    maxActive = Math.max(maxActive, active)
    await new Promise(resolve => setTimeout(resolve, 5))
    active--
    return new Response(null, { status: 204 })
  })

  await Promise.all([
    limitedFetch('https://example.test/1'),
    limitedFetch('https://example.test/2'),
    limitedFetch('https://example.test/3'),
  ])

  check('rate-limited requests use one connection', maxActive === 1, `${maxActive} active`)
  check('rate-limited request starts are spaced apart',
    starts.slice(1).every((start, i) => start - starts[i] >= 20), starts.join(', '))
  check('FOSSGIS routing interval is at least one second', FOSSGIS_REQUEST_INTERVAL_MS >= 1000)
  check('Nominatim interval is at least one second', NOMINATIM_REQUEST_INTERVAL_MS >= 1000)
  check('OSM thumbnails use the policy hostname',
    getThumbnailLayer('openfreemap').url === 'https://tile.openstreetmap.org/{z}/{x}/{y}.png')
  check('service attribution links to fix-the-map',
    SERVICE_ATTRIBUTIONS.some(credit => credit.includes('openstreetmap.org/fixthemap')))
  check('service attribution names elevation sources',
    SERVICE_ATTRIBUTIONS.some(credit => credit.includes('tilezen/joerd')) &&
      SERVICE_ATTRIBUTIONS.some(credit => credit.includes('open-meteo.com')))
  check('fuel prices use the ministry current host',
    FUEL_PRICE_ENDPOINT.startsWith('https://energia.serviciosmin.gob.es/'))
}

/* -- Runtime offline policy ------------------------------------------ */

console.log(`\nRuntime offline checks\n${'='.repeat(78)}`)

{
  const resolved = resolveMapStyleResources({
    version: 8,
    sprite: '/map/openfreemap/sprite',
    glyphs: '/map/openfreemap/glyphs/{fontstack}/{range}.pbf',
    sources: {
      roads: { type: 'vector', url: './source.json', tiles: ['/tiles/{z}/{x}/{y}.pbf'] },
      places: { type: 'geojson', data: './places.geojson' },
    },
    layers: [],
  } as Parameters<typeof resolveMapStyleResources>[0], 'https://maps.example.test/styles/liberty.json')
  const value = resolved as unknown as {
    sprite: string
    glyphs: string
    sources: Record<string, { url?: string; tiles?: string[]; data?: string }>
  }
  check('fetched map styles resolve root-relative sprites and glyph templates',
    value.sprite === 'https://maps.example.test/map/openfreemap/sprite' &&
      value.glyphs === 'https://maps.example.test/map/openfreemap/glyphs/{fontstack}/{range}.pbf')
  check('fetched map styles resolve source, tile, and GeoJSON resource URLs',
    value.sources.roads.url === 'https://maps.example.test/styles/source.json' &&
      value.sources.roads.tiles?.[0] === 'https://maps.example.test/tiles/{z}/{x}/{y}.pbf' &&
      value.sources.places.data === 'https://maps.example.test/styles/places.geojson')
}

{
  const cleared = clearedRouteDerivedState()
  check('route reset removes geometry and route information',
    cleared.coordinates.length === 0 && cleared.durationSeconds === null &&
      cleared.engine === null && cleared.routeCache === undefined)
  check('route reset removes surface results, cache, errors, and activity',
    cleared.surfaceSegments === null && cleared.surfaceCache === undefined &&
      cleared.surfaceError === null && !cleared.surfaceApproximate && !cleared.surfaceLoading)
  check('route reset removes elevation provenance and prior error state',
    !cleared.elevationInterpolated && !cleared.elevationApiError)
  check('route reset removes request status and loading state',
    cleared.routeStatus === '' && !cleared.routedLoading)
  check('only the current non-aborted route sequence may update state',
    routeSequenceIsCurrent(7, 7, false) &&
      !routeSequenceIsCurrent(6, 7, false) &&
      !routeSequenceIsCurrent(7, 7, true))
}

{
  const runtime = decodeRuntimeConfig({
    nominatimUrl: 'https://search.example.test',
    offline: {
      enabled: true,
      mode: 'cache-only',
      status: '/offline/status',
      packs: '/offline/packs',
      modeControl: '/offline/mode',
    },
    services: {
      fuel: '/fuel',
      places: '/places/search',
      pois: 42,
      valhallaRoute: '/routing/valhalla/route',
    },
    maps: {
      raster: {
        osm: '/map/raster/osm/{z}/{x}/{y}.png',
        opentopo: '/map/raster/opentopo/{z}/{x}/{y}.png',
        cyclosm: null,
      },
      openfreemap: { style: '/map/openfreemap/style.json', allowBulk: true },
    },
  }, 'https://backend.example.test/api')

  check('relative API routes are resolved against VITE_API_BASE',
    resolveApiUrl('/places/search', 'https://backend.example.test/api/') ===
      'https://backend.example.test/api/places/search')
  check('absolute advertised routes stay absolute',
    resolveApiUrl('https://tiles.example.test/style.json', 'https://backend.example.test/api') ===
      'https://tiles.example.test/style.json')
  check('config decoding keeps valid optional service routes',
    runtime.services.fuel === 'https://backend.example.test/api/fuel' &&
      runtime.services.places === 'https://backend.example.test/api/places/search' &&
      runtime.services.pois === undefined)
  check('config decoding keeps cache-only mode', runtime.offline?.mode === 'cache-only')
  check('config decoding resolves management routes',
    runtime.offline?.packs === 'https://backend.example.test/api/offline/packs' &&
      runtime.offline.modeControl === 'https://backend.example.test/api/offline/mode')
  let modeRequest: { url?: string; method?: string; body?: string } = {}
  const changedMode = await setRuntimeOfflineMode(runtime, 'auto', async (input, init) => {
    modeRequest = { url: String(input), method: init?.method, body: String(init?.body) }
    return new Response('{"mode":"auto","changed":true}', {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  })
  check('runtime offline changes use the protected advertised endpoint',
    changedMode === 'auto' && modeRequest.url === 'https://backend.example.test/api/offline/mode' &&
      modeRequest.method === 'PUT' && modeRequest.body === '{"mode":"auto"}')

  const standalone = decodeRuntimeConfig(null, '')
  const direct = selectRuntimeTransport(standalone, 'places', 'https://public.example.test/search')
  check('missing backend preserves direct provider fallback',
    direct.kind === 'direct' && direct.url === 'https://public.example.test/search')
  const remoteBootstrap = bootstrapRuntimeConfig('https://backend.example.test/api')
  check('a separately hosted frontend fails closed until backend config loads',
    remoteBootstrap.offline?.mode === 'cache-only' &&
      selectRuntimeTransport(remoteBootstrap, 'places', 'https://public.example.test/search').kind === 'unavailable' &&
      runtimeTerrainLayers(remoteBootstrap).length === 0)

  const unreachable = await loadRuntimeConfig('/api', undefined, async () => {
    throw new TypeError('backend down')
  })
  check('unreachable config decodes as standalone behavior',
    unreachable.offline === undefined &&
      selectRuntimeTransport(unreachable, 'places', 'https://public.example.test/search').kind === 'direct')

  const embeddedCacheOnly = decodeRuntimeConfig({ offline: { enabled: true, mode: 'cache-only' } }, '/api')
  const unavailableManaged = await loadRuntimeConfig('/api', undefined, async () => {
    throw new TypeError('backend config unavailable')
  }, embeddedCacheOnly)
  check('server-rendered cache-only policy survives config failure',
    unavailableManaged.offline?.mode === 'cache-only' &&
      selectRuntimeTransport(unavailableManaged, 'places', 'https://public.example.test/search').kind === 'unavailable' &&
      runtimeTerrainLayers(unavailableManaged).length === 0)

  const backend = selectRuntimeTransport(runtime, 'fuel', 'https://public.example.test/fuel')
  check('an advertised backend endpoint is selected first',
    backend.kind === 'backend' && backend.url === 'https://backend.example.test/api/fuel')
  check('cache-only refuses an unadvertised direct path',
    selectRuntimeTransport(runtime, 'surface', 'https://public.example.test/surface').kind === 'unavailable')

  const layers = runtimeTerrainLayers(runtime)
  const vector = layers.find(layer => layer.id === 'openfreemap')
  const osm = layers.find(layer => layer.id === 'osm')
  const topo = layers.find(layer => layer.id === 'topo')
  const trails = layers.find(layer => layer.id === 'cyclosm')
  check('runtime OpenFreeMap style uses the advertised backend URL',
    vector?.kind === 'vector' && vector.styleUrl === 'https://backend.example.test/api/map/openfreemap/style.json')
  check('runtime config preserves explicit OpenFreeMap bulk permission',
    runtime.maps.openfreemap?.allowBulk === true)
  check('runtime OSM is an explicit manual raster layer',
    osm?.kind === 'raster' && osm.url === 'https://backend.example.test/api/map/raster/osm/{z}/{x}/{y}.png' &&
      vector?.kind === 'vector' && vector.fallback.url === 'https://backend.example.test/api/map/raster/osm/{z}/{x}/{y}.png')
  check('cache-only omits unadvertised raster maps',
    topo?.kind === 'raster' && topo.url.includes('backend.example.test/api/map/raster/opentopo/') &&
      trails === undefined && !layers.some(layer => layer.id === 'satellite' || layer.id === 'relief'))
  const selectedUrls = layers.flatMap(layer => layer.kind === 'vector'
    ? [layer.styleUrl, layer.fallback.url]
    : [layer.url])
  check('cache-only terrain selects no direct public URL',
    selectedUrls.every(url => url.startsWith('https://backend.example.test/api/') || url.startsWith('data:')),
    selectedUrls.join(', '))
  check('cache-only disables unadvertised hillshade', runtimeHillshadeLayer(runtime) === undefined)

  const osmOnly = runtimeTerrainLayers(decodeRuntimeConfig({
    offline: { enabled: true, mode: 'cache-only' },
    maps: { raster: { osm: '/map/osm/{z}/{x}/{y}.png' } },
  }, 'https://backend.example.test'))
  check('cache-only offers advertised OSM without impersonating OpenFreeMap',
    osmOnly[0]?.id === 'osm' && osmOnly[0].kind === 'raster' &&
      osmOnly[0].url.startsWith('https://backend.example.test/'))
  check('an unavailable selected vector layer never silently resolves to raster',
    getBaseLayerFrom(osmOnly, 'openfreemap').id === 'unavailable')

  const noMaps = runtimeTerrainLayers(decodeRuntimeConfig({
    offline: { enabled: true, mode: 'cache-only' },
  }))
  check('cache-only with no advertised maps exposes no network layer', noMaps.length === 0)
  check('an empty cache-only layer set has a non-network thumbnail fallback',
    getThumbnailLayer('openfreemap', noMaps).url.startsWith('data:'))
}

{
  const runtime = decodeRuntimeConfig({
    offline: { enabled: true, mode: 'auto' },
    services: { places: '/places/search' },
  }, 'https://backend.example.test')
  const backendRequests: string[] = []
  let replayedMetadata = ''
  const backendFetch: typeof fetch = async input => {
    backendRequests.push(String(input))
    return new Response(JSON.stringify([
      {
        place_id: 987654,
        display_name: 'Verification Place, Aragón, España',
        lat: '41.654',
        lon: '-0.877',
        boundingbox: ['41.60', '41.70', '-0.95', '-0.80'],
        class: 'place',
        type: 'town',
      },
      { place_id: 2, display_name: 'Malformed result', lat: 'not-a-coordinate', lon: '-0.8' },
    ]), {
      headers: {
        'Content-Type': 'application/json',
        'X-GPX-Cache': 'hit',
      },
    })
  }
  const managed = await searchPlaces('Verification Place 987654', undefined, 'https://public.example.test', {
    runtime,
    fetcher: backendFetch,
  })
  const managedURL = new URL(backendRequests[0])
  check('place search uses the advertised managed endpoint',
    backendRequests.length === 1 && managedURL.origin === 'https://backend.example.test' &&
      managedURL.pathname === '/places/search' && managedURL.searchParams.get('q') === 'Verification Place 987654')
  check('place search validates coordinates and preserves useful result bounds',
    managed.length === 1 && managed[0].lat === 41.654 && managed[0].lon === -0.877 &&
      managed[0].bounds?.south === 41.6 && managed[0].bounds?.east === -0.8 &&
      managed[0].category === 'place' && managed[0].type === 'town')

  const cached = await searchPlaces('  verification   place 987654 ', undefined, 'https://public.example.test', {
    runtime,
    fetcher: backendFetch,
    onCacheMetadata: metadata => { replayedMetadata = metadata.state },
  })
  check('equivalent place queries reuse their browser cache and cache metadata',
    backendRequests.length === 1 && cached === managed && replayedMetadata === 'hit')

  const directRequests: string[] = []
  await searchPlaces('Verification Place 987654', undefined, 'https://public.example.test', {
    runtime: decodeRuntimeConfig(null),
    fetcher: async input => {
      directRequests.push(String(input))
      return new Response('[]', { headers: { 'Content-Type': 'application/json' } })
    },
  })
  const directURL = new URL(directRequests[0])
  check('managed and direct place transports never share browser cache entries',
    directRequests.length === 1 && directURL.origin === 'https://public.example.test' &&
      directURL.pathname === '/search' && directURL.searchParams.get('format') === 'jsonv2')

  let overlongError = ''
  try {
    await searchPlaces('é'.repeat(101), undefined, 'https://public.example.test', {
      runtime: decodeRuntimeConfig(null),
      fetcher: backendFetch,
    })
  } catch (error) {
    overlongError = (error as Error).message
  }
  check('place search applies the backend UTF-8 query limit before fetching',
    overlongError.includes('200 bytes') && backendRequests.length === 1)
}

{
  const runtime = decodeRuntimeConfig({
    offline: { enabled: true, mode: 'cache-only' },
    services: {
      valhallaRoute: '/routing/valhalla',
      osrmRoute: '/routing/osrm',
    },
  }, 'https://backend.example.test')
  const waypoints = [{ lat: 41.6, lon: -0.9 }, { lat: 41.7, lon: -0.8 }]
  const cacheMiss = () => new Response(JSON.stringify({
    code: 'offline_cache_miss',
    detail: 'route is not cached',
    scope: 'routing',
  }), { status: 504, headers: { 'Content-Type': 'application/json' } })
  const valhallaSuccess = () => new Response(JSON.stringify({
    trip: { legs: [{ shape: '??AA' }], summary: { time: 60, length: 1 } },
  }), {
    headers: { 'Content-Type': 'application/json', 'X-GPX-Cache': 'hit' },
  })

  const valhallaCalls: { url: string; costing?: string }[] = []
  const cachedFallbackFetch: typeof fetch = async (input, init) => {
    const body = JSON.parse(String(init?.body ?? '{}')) as { costing?: string }
    valhallaCalls.push({ url: String(input), costing: body.costing })
    return valhallaCalls.length === 1 ? cacheMiss() : valhallaSuccess()
  }
  const fallbackRoute = await calculateRoute(waypoints, 'mixed', undefined, {
    runtime,
    fetcher: cachedFallbackFetch,
  })
  check('a primary route cache miss replays the advertised Valhalla fallback',
    valhallaCalls.length === 2 && valhallaCalls[0].costing === 'motorcycle' &&
      valhallaCalls[1].costing === 'auto' && fallbackRoute.engine === 'Valhalla auto')
  check('cache-only Valhalla replay never calls a direct router',
    valhallaCalls.every(call => call.url === 'https://backend.example.test/routing/valhalla'))
  check('cached Valhalla fallback metadata reaches the route result',
    fallbackRoute.cache?.state === 'hit')

  const roadCalls: string[] = []
  const cachedOsrmFetch: typeof fetch = async input => {
    roadCalls.push(String(input))
    if (roadCalls.length < 3) return cacheMiss()
    return new Response(JSON.stringify({
      routes: [{ geometry: { coordinates: [[-0.9, 41.6], [-0.8, 41.7]] }, duration: 70, distance: 1200 }],
    }), {
      headers: { 'Content-Type': 'application/json', 'X-GPX-Cache': 'hit' },
    })
  }
  const osrmRoute = await calculateRoute(waypoints, 'road', undefined, {
    runtime,
    fetcher: cachedOsrmFetch,
  })
  check('road cache replay reaches the advertised cached OSRM fallback',
    roadCalls.length === 3 && roadCalls[2] === 'https://backend.example.test/routing/osrm' &&
      osrmRoute.engine === 'OSRM driving')
  check('route fallback requests stay on advertised backend URLs',
    roadCalls.every(url => url.startsWith('https://backend.example.test/routing/')))

  let finalMiss: unknown
  let missCalls = 0
  try {
    await calculateRoute(waypoints, 'road', undefined, {
      runtime,
      fetcher: async () => { missCalls++; return cacheMiss() },
    })
  } catch (error) {
    finalMiss = error
  }
  check('all route cache misses retain a typed offline result',
    missCalls === 3 && finalMiss instanceof OfflineCacheMissError && finalMiss.scope === 'routing')

  let hardFailure: unknown
  let hardFailureCalls = 0
  try {
    await calculateRoute(waypoints, 'mixed', undefined, {
      runtime,
      fetcher: async () => {
        hardFailureCalls++
        return new Response(JSON.stringify({ detail: 'backend failed' }), {
          status: 500,
          headers: { 'Content-Type': 'application/json' },
        })
      },
    })
  } catch (error) {
    hardFailure = error
  }
  check('arbitrary route backend failures do not trigger fallback replay',
    hardFailureCalls === 1 && (hardFailure as Error)?.message === 'backend failed')
}

{
  const runtime = decodeRuntimeConfig({
    offline: { enabled: true, mode: 'auto' },
  }, 'https://backend.example.test')
  const requests: string[] = []
  let failure: unknown
  try {
    await fetchElevationProfile(
      [{ lat: 41.6, lon: -0.9 }],
      runtime.apiBase,
      'https://direct-dem.example.test',
      {
        runtime,
        fetcher: async input => {
          requests.push(String(input))
          return new Response(JSON.stringify({ detail: 'DEM backend failed' }), {
            status: 503,
            headers: { 'Content-Type': 'application/json' },
          })
        },
      },
    )
  } catch (error) {
    failure = error
  }
  check('advertised elevation backend 5xx never bypasses to a direct DEM',
    requests.length === 1 && requests[0] === 'https://backend.example.test/elevation/batch')
  check('advertised elevation backend errors reach the caller',
    (failure as Error)?.message === 'DEM backend failed')

  const standalone = decodeRuntimeConfig(null, 'https://backend.example.test')
  const cacheOnly = decodeRuntimeConfig({
    offline: { enabled: true, mode: 'cache-only' },
  }, 'https://backend.example.test')
  const key = elevationPointCacheKey(
    41.6,
    -0.9,
    runtime.apiBase,
    'https://direct-dem.example.test/',
    'srtm30m',
    { runtime },
  )
  check('elevation point keys change with runtime mode and pre-config standalone identity',
    key !== elevationPointCacheKey(
      41.6, -0.9, standalone.apiBase, 'https://direct-dem.example.test/', 'srtm30m', { runtime: standalone },
    ) && key !== elevationPointCacheKey(
      41.6, -0.9, cacheOnly.apiBase, 'https://direct-dem.example.test/', 'srtm30m', { runtime: cacheOnly },
    ))
  check('elevation point keys change with backend, direct provider, and dataset',
    key !== elevationPointCacheKey(
      41.6, -0.9, 'https://other-backend.example.test', 'https://direct-dem.example.test/', 'srtm30m', { runtime },
    ) && key !== elevationPointCacheKey(
      41.6, -0.9, runtime.apiBase, 'https://other-dem.example.test/', 'srtm30m', { runtime },
    ) && key !== elevationPointCacheKey(
      41.6, -0.9, runtime.apiBase, 'https://direct-dem.example.test/', 'copernicus90m', { runtime },
    ))
  check('equivalent elevation source identities produce stable point keys',
    key === elevationPointCacheKey(
      41.6, -0.9, runtime.apiBase, 'https://direct-dem.example.test', 'srtm30m', { runtime },
    ))

  let memoRequests = 0
  const memoFetch: typeof fetch = async () => {
    memoRequests++
    return new Response(JSON.stringify({ results: [{ elevation: memoRequests === 1 ? 111 : 222 }] }), {
      headers: { 'Content-Type': 'application/json' },
    })
  }
  const standaloneElevation = await fetchGroundElevation(
    12.3456,
    -6.789,
    standalone.apiBase,
    'https://direct-dem.example.test',
    'memo-source-test',
    undefined,
    { runtime: standalone, fetcher: memoFetch },
  )
  const managedElevation = await fetchGroundElevation(
    12.3456,
    -6.789,
    runtime.apiBase,
    'https://direct-dem.example.test',
    'memo-source-test',
    undefined,
    { runtime, fetcher: memoFetch },
  )
  check('pre-config elevation memo values cannot bypass the managed runtime transport',
    memoRequests === 2 && standaloneElevation === 111 && managedElevation === 222)
}

{
  const metadata = parseCacheMetadata(new Headers({
    'X-GPX-Cache': 'stale',
    'X-GPX-Cached-At': 'Wed, 26 Aug 2026 10:00:00 GMT',
    Age: '7200',
  }))
  check('cache response metadata is parsed',
    metadata?.state === 'stale' && metadata.stale && metadata.ageSeconds === 7200)
  check('cache timestamps are preserved in a stable form',
    metadata?.cachedAt === '2026-08-26T10:00:00.000Z', metadata?.cachedAt)

  const compactLabels = [
    formatCacheContext({ state: 'hit', stale: false }, true),
    formatCacheContext({ state: 'stale', stale: true }, true),
    formatCacheContext({ state: 'miss', stale: false }, true),
    formatCacheContext({ state: 'revalidated', stale: false }, true),
    formatCacheContext({ state: 'bypass', stale: false }, true),
  ]
  check('compact cache labels distinguish every backend cache state',
    new Set(compactLabels).size === compactLabels.length, compactLabels.join(', '))
  check('a cache miss is labelled fresh rather than cached',
    compactLabels[2] === 'fresh' && compactLabels[2] !== compactLabels[0])
  check('compact cache labels identify hit, stale, revalidated, and bypass responses',
    compactLabels.join(',') === 'cached,stale,fresh,refreshed,live')

  const runtime = decodeRuntimeConfig({
    offline: { enabled: true, mode: 'auto' },
    services: { fuel: '/fuel' },
  }, 'https://backend.example.test')
  let requests = 0
  let callbackMetadata = metadata
  const fakeFetch: typeof fetch = async input => {
    requests++
    check('advertised fetch calls only the backend URL', String(input) === 'https://backend.example.test/fuel')
    return new Response(JSON.stringify({ value: 17 }), {
      status: 503,
      headers: {
        'Content-Type': 'application/json',
        'X-GPX-Cache': 'stale',
        'X-GPX-Cached-At': 'Wed, 26 Aug 2026 10:00:00 GMT',
      },
    })
  }
  const result = await fetchRuntimeService({
    runtime,
    service: 'fuel',
    directUrl: 'https://public.example.test/fuel',
    fetcher: fakeFetch,
    onCacheMetadata: value => { callbackMetadata = value },
  })
  check('an arbitrary backend 5xx never retries the direct provider',
    requests === 1 && result.response.status === 503)
  check('stale metadata reaches the callback and normalized result',
    callbackMetadata?.stale === true && result.cache?.cachedAt === '2026-08-26T10:00:00.000Z')
  check('cache metadata does not alter the provider body',
    (await result.response.json() as { value: number }).value === 17)

  const miss = await responseError(new Response(JSON.stringify({
    detail: 'not cached',
    code: 'offline_cache_miss',
    scope: 'fuel',
  }), { status: 504, headers: { 'Content-Type': 'application/json' } }), 'fallback')
  check('offline cache misses retain their typed scope',
    miss instanceof OfflineCacheMissError && miss.scope === 'fuel' && miss.message === 'not cached')

  let abortRequests = 0
  const abortFetch: typeof fetch = async () => {
    abortRequests++
    throw new DOMException('Aborted', 'AbortError')
  }
  let abortName = ''
  try {
    await fetchRuntimeService({
      runtime,
      service: 'fuel',
      directUrl: 'https://public.example.test/fuel',
      fetcher: abortFetch,
    })
  } catch (error) {
    abortName = (error as Error).name
  }
  check('an aborted backend request never falls back', abortName === 'AbortError' && abortRequests === 1)
}

{
  const status = decodeOfflineStatus({
    mode: 'cache-only',
    writable: false,
    bytes: 1536,
    maxBytes: 1_048_576,
    entries: 12,
    scopes: {
      routing: { bytes: 512, entries: 2, durableBytes: 256 },
      malformed: 'nope',
    },
    elevationTiles: { bytes: 2048, entries: 4 },
    providers: {
      valhalla: { healthy: false, state: 'offline', lastError: 'network down' },
    },
    jobs: [
      { id: 'pack-1', name: 'Pyrenees', status: 'running', done: 4, total: 10 },
      { status: 'missing id' },
    ],
  })
  check('offline status decoding keeps mode and writable state',
    status.mode === 'cache-only' && status.writable === false)
  check('offline status decoding keeps valid scope and legacy elevation values',
    status.scopes.routing?.bytes === 512 && status.elevationTiles?.entries === 4 &&
      status.scopes.malformed === undefined)
  check('offline status decoding tolerates malformed jobs',
    status.jobs.length === 1 && status.jobs[0].done === 4)
  check('offline status decoding preserves provider health',
    status.providers.valhalla?.healthy === false && status.providers.valhalla?.state === 'offline')
  check('storage byte formatting is compact and binary',
    formatBytes(1536) === '1.50 KiB' && formatBytes(undefined) === 'Unknown')
}

{
  const ordinaryBounds = normalizePackBounds(
    { lat: 42.2, lon: 1.4 },
    { lat: 41.8, lon: -0.7 },
  )
  const crossingBounds = normalizePackBounds(
    { lat: 10, lon: 170 },
    { lat: 11, lon: 190 },
  )
  check('drawn map bounds are ordered and preserved in Web Mercator range',
    ordinaryBounds?.south === 41.8 && Math.abs((ordinaryBounds?.west ?? 0) + 0.7) < 1e-10 &&
      ordinaryBounds.north === 42.2 && Math.abs((ordinaryBounds?.east ?? 0) - 1.4) < 1e-10)
  check('drawn map bounds preserve a short antimeridian crossing',
    crossingBounds?.west === 170 && crossingBounds.east === -170)
  check('degenerate and world-spanning map selections are rejected',
    normalizePackBounds({ lat: 1, lon: 2 }, { lat: 1, lon: 3 }) === null &&
      normalizePackBounds({ lat: -10, lon: -180 }, { lat: 10, lon: 180 }) === null)

  check('pack area follows delayed route availability until the user chooses',
    validPackArea(null, false) === 'bbox' && validPackArea(null, true) === 'route')
  check('pack area remains valid when a preferred route disappears',
    validPackArea('route', false) === 'bbox' && validPackArea('route', true) === 'route')
  check('an explicit map-area preference survives route availability',
    validPackArea('bbox', true) === 'bbox')
  check('unedited pack layers follow the active terrain layer',
    syncPackLayers(['openfreemap'], 'topo', false)[0] === 'opentopo')
  check('user-edited pack layers do not follow later terrain changes',
    syncPackLayers(['osm', 'cyclosm'], 'topo', true).join(',') === 'osm,cyclosm')

  const routeRequest = buildPackEstimateRequest({
    name: '  Pyrenees  ',
    area: 'route',
    route: [{ lat: 42.1, lon: -0.4 }, { lat: 42.2, lon: -0.5 }],
    bbox: null,
    paddingKm: 5,
    minZoom: 14,
    maxZoom: 8,
    layers: ['opentopo'],
    scopes: ['routing', 'elevation'],
  })
  const bboxRequest = buildPackEstimateRequest({
    name: 'Pyrenees',
    area: 'bbox',
    route: [],
    bbox: { south: 41, west: -1, north: 42, east: 0 },
    paddingKm: 5,
    minZoom: 8,
    maxZoom: 14,
    layers: ['opentopo'],
    scopes: ['routing', 'elevation'],
  })
  check('pack request building trims names and normalizes zoom order',
    routeRequest?.name === 'Pyrenees' && routeRequest.minZoom === 8 && routeRequest.maxZoom === 14)
  check('pack request building captures the selected area only',
    routeRequest?.route?.length === 2 && routeRequest.bbox === undefined &&
      bboxRequest?.bbox?.join(',') === '41,-1,42,0' && bboxRequest.route === undefined)
  check('pack request building rejects invalid provider and coordinate inputs',
    buildPackEstimateRequest({
      name: 'Invalid layer', area: 'bbox', route: [], bbox: { south: 41, west: -1, north: 42, east: 0 },
      paddingKm: 0, minZoom: 8, maxZoom: 14, layers: ['unknown'], scopes: [],
    }) === null && buildPackEstimateRequest({
      name: 'Invalid latitude', area: 'bbox', route: [], bbox: { south: -90, west: -1, north: 42, east: 0 },
      paddingKm: 0, minZoom: 8, maxZoom: 14, layers: ['openfreemap'], scopes: ['places'],
    }) === null)
  check('pack signatures invalidate estimates when the full area changes',
    packRequestSignature(routeRequest) !== packRequestSignature(bboxRequest))
  check('pack signatures treat layer and scope order as equivalent',
    packRequestSignature(routeRequest) === packRequestSignature(routeRequest ? {
      ...routeRequest,
      layers: [...routeRequest.layers].reverse(),
      scopes: [...routeRequest.scopes].reverse(),
    } : null))

  const automaticRequest = buildAutomaticPackRequest(' Trans-Pyrenees ', Array.from(
    { length: 6000 },
    (_, index) => ({ lat: 42 + index / 1_000_000, lon: -1 + Math.sin(index / 20) / 100 }),
  ))
  check('automatic packs use bounded route-safe defaults',
    automaticRequest?.name === 'Route: Trans-Pyrenees' && automaticRequest.route !== undefined &&
      automaticRequest.automatic === true && automaticRequest.route.length <= 5000 && automaticRequest.layers[0] === 'openfreemap' &&
      automaticRequest.scopes.join(',') === 'elevation,pois,fuel')

  const estimate = decodePackEstimate({
    estimatedBytes: 2048,
    remainingQuota: 4096,
    counts: { elevation: 12 },
    blocked: [{ layer: 'satellite', resource: 'vector-map', reason: 'provider unavailable' }],
  })
  check('pack estimate decoder accepts backend byte and quota field names',
    estimate.bytes === 2048 && estimate.quotaRemaining === 4096)
  check('pack estimate decoder preserves blocked-provider details',
    estimate.blocked[0]?.layer === 'satellite' && estimate.blocked[0]?.resource === 'vector-map' &&
      estimate.blocked[0]?.reason === 'provider unavailable' && estimate.counts.elevation === 12)

  const packs = decodePacks([{
    id: 'pack-1', state: 'complete', done: 1, total: 1,
    bbox: { south: 41, west: 179, north: 42, east: -179 },
    resources: { water: { done: 1, total: 1, failed: 0, bytes: 42, items: 3 } },
  }])
  check('pack summaries preserve per-resource progress and item counts',
    packs[0]?.resources.water?.done === 1 && packs[0]?.resources.water?.bytes === 42 &&
      packs[0]?.resources.water?.items === 3)
  check('pack summaries preserve validated antimeridian area bounds',
    (packs[0] as { bbox?: { south: number; west: number; north: number; east: number } })?.bbox?.west === 179 &&
      (packs[0] as { bbox?: { south: number; west: number; north: number; east: number } })?.bbox?.east === -179)
  const malformedPack = decodePacks([{
    id: 'pack-2', state: 'complete', bbox: { south: -90, west: -1, north: 42, east: 0 }, resources: {},
  }])[0] as { bbox?: unknown }
  check('pack summaries discard malformed area bounds', malformedPack.bbox === undefined)
}

/* -- Surface classification and chunking ---------------------------- */

console.log(`\nSurface checks\n${'='.repeat(78)}`)

{
  check('paved_rough is still sealed road', classifySurface('paved_rough') === 'paved')
  check('gravel groups with compacted', classifySurface('gravel') === 'compacted')
  check('dirt stays its own class', classifySurface('dirt') === 'dirt')
  check('an untagged edge is unknown, not paved', classifySurface(undefined) === 'unknown')
  check('an unrecognised value is unknown', classifySurface('cobblestone_ish') === 'unknown')

  // Distance weighting: two long sealed segments against six short dirt ones.
  // Counting segments would call this route mostly dirt; it is not.
  const line: { lat: number; lon: number }[] = [{ lat: 41.6, lon: -0.9 }]
  const push = (dLat: number) => line.push({ lat: line[line.length - 1].lat + dLat, lon: -0.9 })
  push(0.09); push(0.09)                                   // two long paved
  for (let i = 0; i < 6; i++) push(0.003)                   // six short dirt
  const cum = cumulativeDistanceKm(line)
  const segs = ['paved', 'paved', 'dirt', 'dirt', 'dirt', 'dirt', 'dirt', 'dirt'] as const
  const summary = summarizeSurface([...segs], cum)

  const pavedKm = summary.shares.find(s => s.id === 'paved')?.km ?? 0
  const dirtKm = summary.shares.find(s => s.id === 'dirt')?.km ?? 0
  check('surface shares are weighted by distance, not segment count',
    pavedKm > dirtKm * 5, `paved ${pavedKm.toFixed(2)} km vs dirt ${dirtKm.toFixed(2)} km`)
  check('shares are ordered longest first', summary.shares[0].id === 'paved')
  check('fractions sum to 1',
    Math.abs(summary.shares.reduce((s, x) => s + x.fraction, 0) - 1) < 1e-9)
  check('unpaved distance counts dirt only',
    Math.abs(summary.unpavedKm - dirtKm) < 1e-9)
  check('summary total matches the route length',
    Math.abs(summary.totalKm - cum[cum.length - 1]) < 1e-9)

  // Unknown must not be laundered into either side of the paved split.
  const unknownSummary = summarizeSurface(['paved', 'unknown'], cumulativeDistanceKm(line.slice(0, 3)))
  check('unknown distance is reported separately', unknownSummary.unknownKm > 0)
  check('unknown is not counted as unpaved', unknownSummary.unpavedKm === 0)
  check('unknown is not counted as paved either',
    (unknownSummary.shares.find(s => s.id === 'paved')?.fraction ?? 0) < 0.9)

  console.log(
    `  distance weighting: paved ${pavedKm.toFixed(1)} km / dirt ${dirtKm.toFixed(1)} km ` +
      `→ ${(summary.unpavedFraction * 100).toFixed(0)}% unpaved`,
  )
}

{
  // The trace service rejects any path over 200 km outright, so chunking is
  // what makes long routes work at all. Checked against the longest tracks in
  // the library, whatever they happen to be.
  const longestTracks = [...library]
    .sort((a, b) => b.track.coordinates.length - a.track.coordinates.length)
    .slice(0, 2)
  if (longestTracks.length === 0) {
    skipped++
    console.log('  SKIP  surface chunking — no suitable file in ./gpx')
  }
  for (const { file, track } of longestTracks) {
    const coords = track.coordinates
    const chunks = chunkShape(coords)
    const cum = cumulativeDistanceKm(coords)

    let contiguous = true
    let longest = 0
    let mostPoints = 0
    for (let i = 0; i < chunks.length; i++) {
      const { start, points } = chunks[i]
      const end = start + points.length - 1
      longest = Math.max(longest, cum[end] - cum[start])
      mostPoints = Math.max(mostPoints, points.length)
      // Chunks must overlap by exactly one point, or segments fall in the gap.
      if (i + 1 < chunks.length && chunks[i + 1].start !== end) contiguous = false
    }
    const last = chunks[chunks.length - 1]

    check(`${file} chunks stay under the trace distance limit`,
      longest <= 150 + 1e-9, `longest chunk ${longest.toFixed(1)} km`)
    check(`${file} chunks stay under the trace point limit`,
      mostPoints <= 5000, `${mostPoints} points`)
    check(`${file} chunks are contiguous`, contiguous)
    check(`${file} chunks start at the first point`, chunks[0].start === 0)
    check(`${file} chunks reach the last point`,
      last.start + last.points.length - 1 === coords.length - 1)

    console.log(
      `  ${file.padEnd(24)} ${cum[cum.length - 1].toFixed(0)} km → ${chunks.length} chunks, ` +
        `longest ${longest.toFixed(0)} km / ${mostPoints} pts`,
    )
  }

  check('a two-point shape needs one chunk', chunkShape([
    { lat: 41.6, lon: -0.9 }, { lat: 41.7, lon: -0.9 },
  ]).length === 1)
  check('a degenerate shape produces no chunks', chunkShape([{ lat: 41.6, lon: -0.9 }]).length === 0)
}

console.log(`\n${'='.repeat(78)}`)
const skipNote = skipped > 0 ? ` (${skipped} file-backed group${skipped === 1 ? '' : 's'} skipped — ./gpx is empty)` : ''
console.log(failures === 0
  ? `All ${checks} checks passed.${skipNote}\n`
  : `${failures} of ${checks} checks FAILED.${skipNote}\n`)
process.exit(failures === 0 ? 0 : 1)
