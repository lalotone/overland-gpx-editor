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
} = await import('../src/lib/fuel')

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
