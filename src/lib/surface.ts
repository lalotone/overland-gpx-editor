/* Surface annotations returned inline with Broom routes. */

export type SurfaceClass = 'paved' | 'compacted' | 'dirt' | 'path' | 'unknown'

export interface SurfaceDefinition {
  id: SurfaceClass
  label: string
  color: string
  hint: string
  /** Counted towards the "unpaved" share. `unknown` counts towards neither. */
  unpaved: boolean
}

export const SURFACE_CLASSES: SurfaceDefinition[] = [
  { id: 'paved', label: 'Sealed road', color: '#3b4a5a', hint: 'Asphalt, concrete, paving stones or cobbles', unpaved: false },
  { id: 'compacted', label: 'Gravel / compacted', color: '#c9a227', hint: 'Graded gravel or compacted hardcore — an easy dirt road', unpaved: true },
  { id: 'dirt', label: 'Dirt track', color: '#b5651d', hint: 'Unsurfaced earth or grass — the classic offroad track', unpaved: true },
  { id: 'path', label: 'Path / rough', color: '#a13d2d', hint: 'Sand, mud, clay or similarly difficult ground', unpaved: true },
  { id: 'unknown', label: 'Unknown', color: '#94a3b8', hint: 'No supported surface tag in OSM here — could be anything', unpaved: false },
]

const SURFACE_BY_ID = new Map(SURFACE_CLASSES.map(surface => [surface.id, surface]))

export function surfaceDefinition(id: SurfaceClass): SurfaceDefinition {
  return SURFACE_BY_ID.get(id) ?? SURFACE_CLASSES[SURFACE_CLASSES.length - 1]
}

export function surfaceColor(id: SurfaceClass): string {
  return surfaceDefinition(id).color
}

/** Lookup-normalized OSM surfaces, without guessing from road class or tracktype. */
export function classifySurface(surface: unknown): SurfaceClass {
  switch (surface) {
    case 'asphalt':
    case 'paved':
    case 'concrete':
    case 'paving_stones':
    case 'cobblestone':
    case 'sett':
      return 'paved'
    case 'compacted':
    case 'fine_gravel':
    case 'gravel':
    case 'pebblestone':
      return 'compacted'
    case 'ground':
    case 'dirt':
    case 'earth':
    case 'unpaved':
    case 'grass':
    case 'grass_paver':
      return 'dirt'
    case 'sand':
    case 'mud':
    case 'clay':
    case 'rock':
    case 'impassable':
      return 'path'
    default:
      return 'unknown'
  }
}

export interface SurfaceResult {
  /** `segments[i]` describes coordinates[i] to coordinates[i + 1]. */
  segments: SurfaceClass[]
}

export interface SurfaceShare {
  id: SurfaceClass
  km: number
  fraction: number
}

export interface SurfaceSummary {
  shares: SurfaceShare[]
  totalKm: number
  unpavedKm: number
  unpavedFraction: number
  unknownKm: number
}

export function summarizeSurface(segments: SurfaceClass[], cumKm: number[]): SurfaceSummary {
  const byClass = new Map<SurfaceClass, number>()
  let totalKm = 0
  for (let i = 0; i < segments.length && i + 1 < cumKm.length; i++) {
    const km = cumKm[i + 1] - cumKm[i]
    if (!(km > 0)) continue
    byClass.set(segments[i], (byClass.get(segments[i]) ?? 0) + km)
    totalKm += km
  }
  const shares = SURFACE_CLASSES
    .map(definition => ({ id: definition.id, km: byClass.get(definition.id) ?? 0 }))
    .filter(share => share.km > 0)
    .map(share => ({ ...share, fraction: totalKm > 0 ? share.km / totalKm : 0 }))
    .sort((a, b) => b.km - a.km)
  const unpavedKm = shares
    .filter(share => surfaceDefinition(share.id).unpaved)
    .reduce((sum, share) => sum + share.km, 0)
  return {
    shares,
    totalKm,
    unpavedKm,
    unpavedFraction: totalKm > 0 ? unpavedKm / totalKm : 0,
    unknownKm: byClass.get('unknown') ?? 0,
  }
}
