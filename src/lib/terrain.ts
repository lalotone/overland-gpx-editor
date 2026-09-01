/*
 * Map layers and the colour scales used to read terrain off a track.
 *
 * For dirt riding the two questions are "how steep is this" and "how high am
 * I", so the track can be coloured by gradient or by absolute altitude, and
 * a hillshade relief overlay can be laid over any base map — including
 * satellite, where imagery alone flattens gullies and ridgelines out.
 */

import type { RuntimeConfig } from './offline'

interface BaseLayerCommon {
  id: string
  label: string
  title: string
  attribution: string
  /** True when the layer already draws contour lines. */
  hasContours?: boolean
}

export interface RasterBaseLayerDefinition extends BaseLayerCommon {
  kind: 'raster'
  url: string
  maxZoom: number
}

export interface VectorBaseLayerDefinition extends BaseLayerCommon {
  kind: 'vector'
  styleUrl: string
  fallback: ThumbnailLayerDefinition
}

export type BaseLayerDefinition = RasterBaseLayerDefinition | VectorBaseLayerDefinition

export interface ThumbnailLayerDefinition {
  url: string
  attribution: string
  maxZoom: number
}

const OSM_ATTRIBUTION =
  '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'

/** Credits for non-tile services whose data remains visible over any base map. */
export const SERVICE_ATTRIBUTIONS = [
  'OSM services: Data &copy; ' +
    '<a href="https://www.openstreetmap.org/copyright">OpenStreetMap contributors</a> ' +
    '(<a href="https://opendatacommons.org/licenses/odbl/">ODbL</a>) | ' +
    '<a href="https://www.openstreetmap.org/fixthemap">fix/report</a>',
  'Elevation: <a href="https://github.com/tilezen/joerd/blob/master/docs/attribution.md">' +
    'USGS, Copernicus &amp; other terrain sources</a> | ' +
    '<a href="https://open-meteo.com/">data by Open-Meteo.com</a>',
]

const OSM_THUMBNAIL_LAYER: ThumbnailLayerDefinition = {
  url: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png',
  attribution: OSM_ATTRIBUTION,
  maxZoom: 19,
}

const EMPTY_TILE_LAYER: RasterBaseLayerDefinition = {
  id: 'unavailable',
  label: 'Offline',
  title: 'No cached map layer is available',
  kind: 'raster',
  url: 'data:image/gif;base64,R0lGODlhAQABAAD/ACwAAAAAAQABAAACADs=',
  attribution: '',
  maxZoom: 19,
}

export const BASE_LAYERS: BaseLayerDefinition[] = [
  {
    id: 'openfreemap',
    label: 'OFM',
    title: 'OpenFreeMap Liberty — fast, sharp vector map',
    kind: 'vector',
    styleUrl: 'https://tiles.openfreemap.org/styles/liberty',
    attribution:
      '<a href="https://openfreemap.org">OpenFreeMap</a> | ' +
      '&copy; <a href="https://www.openmaptiles.org/">OpenMapTiles</a> | ' +
      'Data from <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a>',
    fallback: OSM_THUMBNAIL_LAYER,
  },
  {
    id: 'osm',
    label: 'OSM',
    title: 'OpenStreetMap Standard — raster map',
    kind: 'raster',
    url: OSM_THUMBNAIL_LAYER.url,
    attribution: OSM_THUMBNAIL_LAYER.attribution,
    maxZoom: OSM_THUMBNAIL_LAYER.maxZoom,
  },
  {
    id: 'topo',
    label: 'Topo',
    title: 'OpenTopoMap — contour lines and shaded relief',
    kind: 'raster',
    url: 'https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png',
    attribution:
      'Map data: &copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors, SRTM | ' +
      'Style: &copy; <a href="https://opentopomap.org">OpenTopoMap</a> (CC-BY-SA)',
    maxZoom: 17,
    hasContours: true,
  },
  {
    id: 'cyclosm',
    label: 'Trails',
    title: 'CyclOSM — renders track surface and grade clearly',
    kind: 'raster',
    url: 'https://{s}.tile-cyclosm.openstreetmap.fr/cyclosm/{z}/{x}/{y}.png',
    attribution:
      '<a href="https://github.com/cyclosm/cyclosm-cartocss-style/releases">CyclOSM</a> | ' +
      '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
    maxZoom: 20,
  },
  {
    id: 'satellite',
    label: 'Sat',
    title: 'Esri World Imagery — check a track really exists on the ground',
    kind: 'raster',
    url: 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}',
    attribution: 'Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community',
    maxZoom: 19,
  },
  {
    id: 'relief',
    label: 'Relief',
    title: 'Esri World Shaded Relief — landform shape without clutter',
    kind: 'raster',
    url: 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Shaded_Relief/MapServer/tile/{z}/{y}/{x}',
    attribution: 'Copyright &copy; 2014 Esri',
    maxZoom: 13,
  },
]

/** Semi-transparent relief, drawn between the base tiles and the track. */
export const HILLSHADE_LAYER = {
  url: 'https://server.arcgisonline.com/ArcGIS/rest/services/Elevation/World_Hillshade/MapServer/tile/{z}/{y}/{x}',
  attribution:
    '<a href="https://goto.arcgisonline.com/maps/Elevation/World_Hillshade" ' +
    'title="Sources: Esri, Vantor, Airbus DS, USGS, NGA, NASA, CGIAR, N Robinson, NCEAS, NLS, OS, NMA, ' +
    'Geodatastyrelsen, Rijkswaterstaat, GSA, Geoland, FEMA, Intermap, and the GIS user community">' +
    'Hillshade &copy; Esri and data providers</a>',
  maxZoom: 16,
}

export function getBaseLayer(id: string): BaseLayerDefinition {
  return getBaseLayerFrom(BASE_LAYERS, id)
}

/** Library cards stay as image tiles instead of creating a WebGL map per card. */
export function getThumbnailLayer(
  id: string,
  layers: BaseLayerDefinition[] = BASE_LAYERS,
): ThumbnailLayerDefinition {
  const layer = getBaseLayerFrom(layers, id)
  return layer.kind === 'raster' ? layer : layer.fallback
}

export function getBaseLayerFrom(
  layers: BaseLayerDefinition[],
  id: string,
): BaseLayerDefinition {
  return layers.find(layer => layer.id === id) ?? EMPTY_TILE_LAYER
}

/** Runtime routes replace only adapters explicitly advertised by the backend. */
export function runtimeTerrainLayers(runtime?: RuntimeConfig): BaseLayerDefinition[] {
  const raster = runtime?.maps.raster
  const osm = raster?.osm
  if (runtime?.offline?.mode === 'cache-only') {
    const layers: BaseLayerDefinition[] = []
    const vector = BASE_LAYERS.find(layer => layer.id === 'openfreemap') as VectorBaseLayerDefinition
    const styleUrl = runtime.maps.openfreemap?.style
    if (styleUrl) {
      layers.push({
        ...vector,
        styleUrl,
        fallback: osm ? { ...vector.fallback, url: osm } : EMPTY_TILE_LAYER,
      })
    }
    const osmLayer = BASE_LAYERS.find(layer => layer.id === 'osm')
    if (osmLayer?.kind === 'raster' && osm) {
      layers.push({ ...osmLayer, url: osm })
    }

    const topo = BASE_LAYERS.find(layer => layer.id === 'topo')
    if (topo?.kind === 'raster' && raster?.opentopo) {
      layers.push({ ...topo, url: raster.opentopo })
    }
    const cyclosm = BASE_LAYERS.find(layer => layer.id === 'cyclosm')
    if (cyclosm?.kind === 'raster' && raster?.cyclosm) {
      layers.push({ ...cyclosm, url: raster.cyclosm })
    }
    return layers
  }

  return BASE_LAYERS.map(layer => {
    if (layer.kind === 'vector') {
      return {
        ...layer,
        styleUrl: runtime?.maps.openfreemap?.style ?? layer.styleUrl,
      }
    }
    if (layer.id === 'osm' && osm) return { ...layer, url: osm }
    if (layer.id === 'topo' && raster?.opentopo) return { ...layer, url: raster.opentopo }
    if (layer.id === 'cyclosm' && raster?.cyclosm) return { ...layer, url: raster.cyclosm }
    return layer
  })
}

export function runtimeHillshadeLayer(runtime?: RuntimeConfig): typeof HILLSHADE_LAYER | undefined {
  return runtime?.offline?.mode === 'cache-only' ? undefined : HILLSHADE_LAYER
}

/* -- Gradient colouring ---------------------------------------------- */

export const SLOPE_COLORS = [
  '#d63031', // very steep up
  '#e17055', // steep up
  '#f0932b', // moderate up
  '#6ab04c', // gentle up
  '#a0cfe0', // flat
  '#74b9ff', // gentle down
  '#0984e3', // moderate down
  '#2d3aff', // steep down
] as const

export const SLOPE_LABELS = [
  'Very steep up (>15%)',
  'Steep up (8-15%)',
  'Moderate up (2-8%)',
  'Gentle up (0-2%)',
  'Flat (-2% to 2%)',
  'Gentle down (-2% to -8%)',
  'Moderate down (-8% to -15%)',
  'Steep down (<-15%)',
] as const

export function getSegmentColor(slope: number): string {
  if (slope > 15) return SLOPE_COLORS[0]
  if (slope > 8) return SLOPE_COLORS[1]
  if (slope > 2) return SLOPE_COLORS[2]
  if (slope >= 0) return SLOPE_COLORS[3]
  if (slope >= -2) return SLOPE_COLORS[4]
  if (slope >= -8) return SLOPE_COLORS[5]
  if (slope >= -15) return SLOPE_COLORS[6]
  return SLOPE_COLORS[7]
}

/* -- Altitude colouring (hypsometric tints) --------------------------- */

/**
 * Classic cartographic elevation ramp: valley green through to rock/snow.
 * Stops are positions 0..1 across the track's own min/max, so the scale
 * always uses its full range whether the ride is in the Monegros or Pyrenees.
 */
const ALTITUDE_RAMP: { t: number; rgb: [number, number, number] }[] = [
  { t: 0.0,  rgb: [ 40, 122,  70] }, // deep green
  { t: 0.18, rgb: [110, 172,  90] }, // upland green
  { t: 0.36, rgb: [200, 214, 118] }, // dry grass
  { t: 0.54, rgb: [235, 205, 120] }, // sand
  { t: 0.70, rgb: [206, 154,  84] }, // ochre
  { t: 0.85, rgb: [160, 104,  70] }, // rock brown
  { t: 1.0,  rgb: [238, 236, 232] }, // bare rock / snow
]

export const ALTITUDE_LEGEND_STOPS = ALTITUDE_RAMP.map(
  s => ({ t: s.t, color: `rgb(${s.rgb[0]}, ${s.rgb[1]}, ${s.rgb[2]})` }),
)

const rampStops = ALTITUDE_LEGEND_STOPS
  .map(s => `${s.color} ${(s.t * 100).toFixed(0)}%`)
  .join(', ')

/** Horizontal altitude legend bar (low on the left). */
export const ALTITUDE_GRADIENT_CSS = `linear-gradient(to right, ${rampStops})`

/** Vertical altitude legend bar (low at the bottom, as on a map legend). */
export const ALTITUDE_GRADIENT_CSS_VERTICAL = `linear-gradient(to top, ${rampStops})`

export function altitudeColor(elevation: number, min: number, max: number): string {
  const range = max - min
  const t = range > 0 ? Math.min(1, Math.max(0, (elevation - min) / range)) : 0

  let lo = ALTITUDE_RAMP[0]
  let hi = ALTITUDE_RAMP[ALTITUDE_RAMP.length - 1]
  for (let i = 0; i < ALTITUDE_RAMP.length - 1; i++) {
    if (t >= ALTITUDE_RAMP[i].t && t <= ALTITUDE_RAMP[i + 1].t) {
      lo = ALTITUDE_RAMP[i]
      hi = ALTITUDE_RAMP[i + 1]
      break
    }
  }

  const span = hi.t - lo.t
  const k = span > 0 ? (t - lo.t) / span : 0
  const mix = (a: number, b: number) => Math.round(a + (b - a) * k)
  return `rgb(${mix(lo.rgb[0], hi.rgb[0])}, ${mix(lo.rgb[1], hi.rgb[1])}, ${mix(lo.rgb[2], hi.rgb[2])})`
}

/**
 * How the track line is coloured. `surface` needs per-segment data from
 * `lib/surface.ts` and is only offered where that has been fetched.
 */
export type ColorMode = 'slope' | 'altitude' | 'surface'
