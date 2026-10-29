/*
 * Legal-access warnings from the OSM tags Broom returns per edge.
 *
 * Broom hands back lookup-normalized tags, not a verdict: it does not evaluate
 * the access hierarchy, `:conditional` restrictions, barriers or national
 * defaults. So this module evaluates only what the tags say, for a motorcycle,
 * and is explicit about everything it cannot know:
 *
 * - The most specific supplied key wins: motorcycle, then motor_vehicle, then
 *   vehicle, then access. `bicycle`/`foot` permission never counts.
 * - A way with no access tag is `untagged`, never "open". Its legality comes
 *   from the national default for that road class, which differs by country
 *   (forest tracks are closed by regional law in parts of Spain, open in much
 *   of France), so it is counted on its own and never folded into "open".
 * - A path-like way with no motor permission is flagged: the OSM default for
 *   paths, footways, bridleways and cycleways excludes motor vehicles almost
 *   everywhere. This is the same rule Broom's offroad profiles route by.
 * - Distance that could not be matched to a road has no tags at all and is
 *   `unknown`, never assumed fine.
 *
 * Gates and seasonal closures are not visible here: barriers are OSM nodes,
 * and conditional access is not part of Broom's attributes.
 */

/** Lookup-normalized OSM tags as Broom returns them; empty means absent. */
export interface RoadTags {
  highway?: string
  access?: string
  vehicle?: string
  motor_vehicle?: string
  motorcycle?: string
}

export type AccessClass =
  | 'closed'
  | 'private'
  | 'restricted'
  | 'path'
  | 'open'
  | 'untagged'
  | 'unknown'

export interface AccessDefinition {
  id: AccessClass
  label: string
  color: string
  hint: string
  /** Shown as a warning stretch on the map and in the list. */
  warn: boolean
}

export const ACCESS_CLASSES: AccessDefinition[] = [
  { id: 'closed', label: 'Closed to motorcycles', color: '#c0392b', warn: true,
    hint: 'Tagged no for motorcycles or motor vehicles' },
  { id: 'private', label: 'Private', color: '#8e2a5c', warn: true,
    hint: 'Tagged private — only with the owner’s permission' },
  { id: 'restricted', label: 'Restricted use', color: '#e67e22', warn: true,
    hint: 'Forestry, agricultural, destination, permit or similar — not general traffic' },
  { id: 'path', label: 'Path without motor permission', color: '#d4a017', warn: true,
    hint: 'A path, footway, bridleway or cycleway with no motor tag; these exclude motor vehicles by default almost everywhere' },
  { id: 'open', label: 'Explicitly open', color: '#2e8b57', warn: false,
    hint: 'Tagged yes, permissive or designated for motor traffic' },
  { id: 'untagged', label: 'No access tag', color: '#94a3b8', warn: false,
    hint: 'No access tag in OSM — the national default for this road class applies' },
  { id: 'unknown', label: 'Not matched to a road', color: '#cbd5e1', warn: false,
    hint: 'This stretch could not be matched to the routing data, so nothing is known about it' },
]

const ACCESS_BY_ID = new Map(ACCESS_CLASSES.map(definition => [definition.id, definition]))

export function accessDefinition(id: AccessClass): AccessDefinition {
  return ACCESS_BY_ID.get(id) ?? ACCESS_CLASSES[ACCESS_CLASSES.length - 1]
}

export interface AccessVerdict {
  id: AccessClass
  /** The deciding tag, e.g. `motor_vehicle=forestry`; absent when untagged. */
  tag?: string
  highway?: string
}

const ACCESS_KEYS = ['motorcycle', 'motor_vehicle', 'vehicle', 'access'] as const

/** Highway classes whose OSM default excludes motor vehicles. */
const PATH_HIGHWAYS = new Set(['path', 'footway', 'cycleway', 'bridleway', 'pedestrian', 'steps'])

function classifyValue(value: string): AccessClass {
  switch (value) {
    case 'yes':
    case 'permissive':
    case 'designated':
      return 'open'
    case 'no':
      return 'closed'
    case 'private':
      return 'private'
    // Broom's `unknown` is a value outside its lookup table: real, but unreadable.
    case 'unknown':
      return 'unknown'
    default:
      // destination, customers, delivery, agricultural, forestry, permit,
      // discouraged, military, emergency… all mean "not for passing through".
      return 'restricted'
  }
}

export function classifyAccess(road: RoadTags | null | undefined): AccessVerdict {
  if (!road) return { id: 'unknown' }
  const highway = road.highway || undefined
  for (const key of ACCESS_KEYS) {
    const value = road[key]
    if (!value) continue
    // General `access=yes` says nothing about motors on a path.
    if (key === 'access' && highway && PATH_HIGHWAYS.has(highway) && classifyValue(value) === 'open') {
      return { id: 'path', tag: `${key}=${value}`, highway }
    }
    return { id: classifyValue(value), tag: `${key}=${value}`, highway }
  }
  if (highway && PATH_HIGHWAYS.has(highway)) return { id: 'path', highway }
  return { id: 'untagged', highway }
}

/**
 * A stretch of the original track carrying one set of tags. Positions address
 * the track's own points: `startIndex` + `startFraction` of the way to the
 * next point, exactly as Broom reports them.
 */
export interface LocatedRoad {
  startIndex: number
  startFraction: number
  endIndex: number
  endFraction: number
  road: RoadTags | null
}

export interface AccessStretch {
  id: AccessClass
  tag?: string
  highway?: string
  startKm: number
  endKm: number
  /** Inclusive coordinate range covering the stretch, for drawing it. */
  startIndex: number
  endIndex: number
}

export interface AccessReport {
  /** Warning stretches only, in track order. */
  stretches: AccessStretch[]
  /** Kilometres per class across the whole track. */
  km: Partial<Record<AccessClass, number>>
  totalKm: number
  warnKm: number
}

function kmAt(cumKm: number[], index: number, fraction: number): number {
  const last = cumKm.length - 1
  if (index >= last) return cumKm[last] ?? 0
  const base = cumKm[Math.max(0, index)] ?? 0
  return base + (cumKm[index + 1] - base) * fraction
}

export function accessReport(spans: LocatedRoad[], cumKm: number[]): AccessReport {
  const km: Partial<Record<AccessClass, number>> = {}
  const stretches: AccessStretch[] = []
  let totalKm = 0
  for (const span of spans) {
    const startKm = kmAt(cumKm, span.startIndex, span.startFraction)
    const endKm = kmAt(cumKm, span.endIndex, span.endFraction)
    const length = endKm - startKm
    if (!(length > 0)) continue
    const verdict = classifyAccess(span.road)
    km[verdict.id] = (km[verdict.id] ?? 0) + length
    totalKm += length
    if (!accessDefinition(verdict.id).warn) continue
    const endIndex = Math.min(cumKm.length - 1, span.endFraction > 0 ? span.endIndex + 1 : span.endIndex)
    const previous = stretches[stretches.length - 1]
    if (previous && previous.id === verdict.id && previous.tag === verdict.tag &&
        Math.abs(previous.endKm - startKm) < 1e-9) {
      previous.endKm = endKm
      previous.endIndex = endIndex
      continue
    }
    stretches.push({
      ...verdict,
      startKm,
      endKm,
      startIndex: Math.max(0, span.startIndex),
      endIndex,
    })
  }
  const warnKm = stretches.reduce((sum, stretch) => sum + stretch.endKm - stretch.startKm, 0)
  return { stretches, km, totalKm, warnKm }
}

const ROAD_TAG_KEYS = ['highway', 'access', 'vehicle', 'motor_vehicle', 'motorcycle'] as const

/** Keeps only the string tags this module reads; anything else is dropped. */
export function decodeRoadTags(raw: unknown): RoadTags | null {
  if (raw === null || typeof raw !== 'object') return null
  const source = raw as Record<string, unknown>
  const road: RoadTags = {}
  for (const key of ROAD_TAG_KEYS) {
    if (typeof source[key] === 'string' && source[key]) road[key] = source[key] as string
  }
  return road
}

/** Per-segment tags from a planned route: `roads[i]` covers points i..i+1. */
export function locateSegmentRoads(roads: (RoadTags | null)[]): LocatedRoad[] {
  return roads.map((road, i) => ({ startIndex: i, startFraction: 0, endIndex: i + 1, endFraction: 0, road }))
}
