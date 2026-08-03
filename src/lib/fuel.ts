/*
 * Spanish official fuel prices.
 *
 * The Ministerio de Industria publishes every filling station in Spain with
 * the price of each fuel it sells, updated through the day. That is strictly
 * better than the OSM fuel layer inside Spain — it is the authoritative list,
 * and it carries prices and opening hours that OSM mostly does not.
 *
 * The service answers with CORS wide open, so this runs straight from the
 * browser with no proxy. What it does not do is filter by bounding box: the
 * only filters are province, municipality and región, none of which a map
 * viewport maps onto cleanly (a 100 km view routinely straddles three
 * provinces). So the whole country comes down once, is cached for the
 * session, and is filtered here. It is ~12 MB and about five seconds on a
 * decent line — paid once, and only if the user actually asks for fuel.
 *
 * Numbers arrive as Spanish-formatted strings ("1,819", "41,840056"), and an
 * empty string means "does not sell this fuel" — never zero.
 *
 * The feed carries public-sale stations only: `Tipo Venta` is "P" for all
 * 11,488 of them, so there is nothing to filter out for restricted co-op or
 * fleet pumps, and nothing here pretends to.
 */

import { haversineDistance } from './geo'
import type { BoundingBox, Poi, PoiDetail } from './poi'

const ENDPOINT =
  'https://sedeaplicaciones.minetur.gob.es/ServiciosRESTCarburantes/PreciosCarburantes/EstacionesTerrestres/'

/**
 * Rough envelope of Spanish territory. Only used to decide whether it is
 * worth asking this service at all, so generous edges are harmless — a view
 * of southern France simply falls back to OSM.
 */
const SPAIN_AREAS: BoundingBox[] = [
  { south: 35.0, west: -9.5, north: 44.0, east: 4.5 },    // mainland, Baleares, Ceuta y Melilla
  { south: 27.5, west: -18.3, north: 29.5, east: -13.3 }, // Canarias
]

export function intersectsSpain(bbox: BoundingBox): boolean {
  return SPAIN_AREAS.some(
    area =>
      bbox.south <= area.north &&
      bbox.north >= area.south &&
      bbox.west <= area.east &&
      bbox.east >= area.west,
  )
}

/*
 * The fuels worth showing on a motorbike/overlanding map. The feed carries
 * about thirty, most of them blank almost everywhere (hydrogen, methanol,
 * biogas), and listing them all would bury the two prices that matter.
 */
const PRICE_FIELDS: { field: string; label: string }[] = [
  { field: 'Precio Gasolina 95 E5', label: 'Gasolina 95' },
  { field: 'Precio Gasolina 98 E5', label: 'Gasolina 98' },
  { field: 'Precio Gasoleo A', label: 'Gasóleo A' },
  { field: 'Precio Gasoleo Premium', label: 'Gasóleo Premium' },
]

export interface FuelPrice {
  label: string
  /** Euros per litre. */
  price: number
}

/** The fuel prices are ranked against by default — what most bikes take. */
export const DEFAULT_FUEL_REFERENCE = 'Gasolina 95'

/**
 * Cheapest to dearest. Five bands rather than a continuous ramp: the marker
 * colours have to be told apart at a glance on a busy map, and it keeps the
 * cached icon set small.
 */
export const FUEL_PRICE_BANDS: { label: string; color: string }[] = [
  { label: 'Cheapest', color: '#16a34a' },
  { label: 'Cheaper', color: '#84cc16' },
  { label: 'Average', color: '#eab308' },
  { label: 'Dearer', color: '#f97316' },
  { label: 'Dearest', color: '#dc2626' },
]

/** Stations that do not sell the fuel being ranked. */
export const FUEL_NO_PRICE_COLOR = '#94a3b8'

/**
 * Band each price by rank within the set, cheapest band first.
 *
 * Rank, not min/max: one motorway station at 2.40 € would otherwise squash
 * every ordinary station into the green end and the colouring would stop
 * saying anything. Ranking always spends the whole scale on the stations
 * actually in view, which is the comparison a rider is making.
 */
export function priceBands(values: number[], bandCount = FUEL_PRICE_BANDS.length): number[] {
  const n = values.length
  const middle = Math.floor((bandCount - 1) / 2)
  if (n === 0) return []

  const min = Math.min(...values)
  const max = Math.max(...values)
  // One station, or every station at the same price: there is no cheaper or
  // dearer to show, and painting them all green would invent a bargain.
  if (n === 1 || max === min) return values.map(() => middle)

  const sorted = [...values].sort((a, b) => a - b)
  return values.map(value => {
    // Count strictly cheaper, so equal prices always land in the same band.
    let lo = 0
    let hi = sorted.length
    while (lo < hi) {
      const mid = (lo + hi) >> 1
      if (sorted[mid] < value) lo = mid + 1
      else hi = mid
    }
    const t = lo / (n - 1)
    return Math.max(0, Math.min(bandCount - 1, Math.floor(t * bandCount)))
  })
}

/**
 * Marker colour per POI id, ranking on `reference` among those that sell it.
 * POIs with no price for that fuel are left neutral rather than ranked
 * against a fuel they do not stock.
 */
export function fuelBandColors(pois: Poi[], reference: string): Map<string, string> {
  const priced = pois.filter(p => typeof p.prices?.[reference] === 'number')
  // No prices at all means these came from OSM, not the ministry: leave the
  // layer its own colour rather than painting every marker "unknown".
  if (priced.length === 0) return new Map()

  const bands = priceBands(priced.map(p => p.prices![reference]))

  const colors = new Map<string, string>()
  for (const poi of pois) colors.set(poi.id, FUEL_NO_PRICE_COLOR)
  priced.forEach((poi, i) => colors.set(poi.id, FUEL_PRICE_BANDS[bands[i]].color))
  return colors
}

/** Fuels actually on offer across a set, in the order they are displayed. */
export function availableFuels(pois: Poi[]): string[] {
  const seen = new Set<string>()
  for (const poi of pois) {
    for (const label of Object.keys(poi.prices ?? {})) seen.add(label)
  }
  return PRICE_FIELDS.map(f => f.label).filter(label => seen.has(label))
}

export interface FuelStation {
  id: string
  lat: number
  lon: number
  /** Brand, e.g. REPSOL, CEPSA — the feed's `Rótulo`. */
  brand: string
  address: string
  town: string
  hours: string
  prices: FuelPrice[]
}

/** Spanish decimal comma, and an empty field meaning "not sold". */
function toNumber(raw: string | undefined): number | null {
  if (!raw) return null
  const value = Number(raw.replace(',', '.'))
  return Number.isFinite(value) ? value : null
}

export type RawStation = Record<string, string>

/** Exported so the verify harness can pin the format without the network. */
export function parseFuelStations(list: RawStation[]): FuelStation[] {
  const out: FuelStation[] = []
  for (const raw of list) {
    const station = parseStation(raw)
    if (station) out.push(station)
  }
  return out
}

function parseStation(raw: RawStation): FuelStation | null {
  const lat = toNumber(raw['Latitud'])
  const lon = toNumber(raw['Longitud (WGS84)'])
  if (lat === null || lon === null) return null

  const prices: FuelPrice[] = []
  for (const { field, label } of PRICE_FIELDS) {
    const price = toNumber(raw[field])
    if (price !== null && price > 0) prices.push({ label, price })
  }

  return {
    id: `eess/${raw['IDEESS']}`,
    lat,
    lon,
    brand: (raw['Rótulo'] ?? '').trim(),
    address: (raw['Dirección'] ?? '').trim(),
    town: (raw['Municipio'] ?? '').trim(),
    hours: (raw['Horario'] ?? '').trim(),
    prices,
  }
}

export interface FuelDataset {
  stations: FuelStation[]
  /** Publication timestamp as the ministry states it, e.g. "02/08/2026 23:15". */
  published: string
}

let cached: FuelDataset | null = null
let inflight: Promise<FuelDataset> | null = null

async function download(): Promise<FuelDataset> {
  const res = await fetch(ENDPOINT)
  if (!res.ok) throw new Error(`Fuel price service returned ${res.status}`)

  const data = (await res.json()) as {
    ListaEESSPrecio?: RawStation[]
    Fecha?: string
    ResultadoConsulta?: string
  }
  const list = data.ListaEESSPrecio
  if (!Array.isArray(list)) throw new Error('Fuel price service returned an unexpected format')

  return { stations: parseFuelStations(list), published: (data.Fecha ?? '').trim() }
}

/**
 * The national dataset, downloaded at most once per session.
 *
 * The caller's abort signal deliberately does not cancel the download: one
 * view being abandoned should not throw away a 12 MB fetch that every later
 * view will want. It is honoured for the caller's own result instead.
 */
export async function loadFuelDataset(signal?: AbortSignal): Promise<FuelDataset> {
  if (!cached) {
    if (!inflight) {
      inflight = download().finally(() => { inflight = null })
    }
    cached = await inflight
  }
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError')
  return cached
}

function detailFor(station: FuelStation, published: string): PoiDetail {
  return {
    lines: station.prices.map(p => ({ label: p.label, value: `${p.price.toFixed(3)} €/L` })),
    note: [station.address, station.hours && `Horario: ${station.hours}`]
      .filter(Boolean)
      .join(' · '),
    source: published
      ? `Ministerio de Industria · ${published}`
      : 'Ministerio de Industria',
  }
}

/**
 * Stations inside `bbox`, nearest the middle of the view first.
 *
 * Capped like the Overpass layer: a wide view over a city can hold several
 * hundred pumps, and past a point they stop being a map and start being a
 * wall of markers.
 */
export async function fetchSpanishFuelPois(
  bbox: BoundingBox,
  limit: number,
  signal?: AbortSignal,
): Promise<{ pois: Poi[]; published: string; truncated: boolean }> {
  const { stations, published } = await loadFuelDataset(signal)

  const inside = stations.filter(
    s => s.lat >= bbox.south && s.lat <= bbox.north && s.lon >= bbox.west && s.lon <= bbox.east,
  )

  const centre = { lat: (bbox.south + bbox.north) / 2, lon: (bbox.west + bbox.east) / 2 }
  inside.sort(
    (a, b) =>
      haversineDistance(centre, { lat: a.lat, lon: a.lon }) -
      haversineDistance(centre, { lat: b.lat, lon: b.lon }),
  )

  const truncated = inside.length > limit
  const pois: Poi[] = inside.slice(0, limit).map(station => ({
    id: station.id,
    kind: 'fuel' as const,
    lat: station.lat,
    lon: station.lon,
    name: [station.brand, station.town].filter(Boolean).join(' — ') || undefined,
    detail: detailFor(station, published),
    prices: Object.fromEntries(station.prices.map(p => [p.label, p.price])),
  }))

  return { pois, published, truncated }
}
