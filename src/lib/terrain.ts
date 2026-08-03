/*
 * Map layers and the colour scales used to read terrain off a track.
 *
 * For dirt riding the two questions are "how steep is this" and "how high am
 * I", so the track can be coloured by gradient or by absolute altitude, and
 * a hillshade relief overlay can be laid over any base map — including
 * satellite, where imagery alone flattens gullies and ridgelines out.
 */

export interface BaseLayerDefinition {
  id: string
  label: string
  title: string
  url: string
  attribution: string
  maxZoom: number
  /** True when the layer already draws contour lines. */
  hasContours?: boolean
}

export const BASE_LAYERS: BaseLayerDefinition[] = [
  {
    id: 'osm',
    label: 'Map',
    title: 'OpenStreetMap — standard road map',
    url: 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png',
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
    maxZoom: 19,
  },
  {
    id: 'topo',
    label: 'Topo',
    title: 'OpenTopoMap — contour lines and shaded relief',
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
    url: 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}',
    attribution: 'Tiles &copy; Esri — Esri, i-cubed, USDA, USGS, AEX, GeoEye, Getmapping, Aerogrid, IGN, IGP, UPR-EGP',
    maxZoom: 19,
  },
  {
    id: 'relief',
    label: 'Relief',
    title: 'Esri World Shaded Relief — landform shape without clutter',
    url: 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Shaded_Relief/MapServer/tile/{z}/{y}/{x}',
    attribution: 'Tiles &copy; Esri — Source: Esri',
    maxZoom: 13,
  },
]

/** Semi-transparent relief, drawn between the base tiles and the track. */
export const HILLSHADE_LAYER = {
  url: 'https://server.arcgisonline.com/ArcGIS/rest/services/Elevation/World_Hillshade/MapServer/tile/{z}/{y}/{x}',
  attribution: 'Hillshade &copy; Esri — Esri, USGS, NOAA',
  maxZoom: 16,
}

export function getBaseLayer(id: string): BaseLayerDefinition {
  return BASE_LAYERS.find(l => l.id === id) ?? BASE_LAYERS[0]
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
