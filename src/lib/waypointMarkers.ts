export const WAYPOINT_MARKER_IDS = [
  'generic',
  'fuel',
  'water',
  'camp',
  'food',
  'lodging',
  'parking',
  'repair',
  'medical',
  'viewpoint',
  'hazard',
  'roadblock',
  'ferry',
  'border',
  'restroom',
  'information',
  'picnic',
] as const

export type WaypointMarkerId = typeof WAYPOINT_MARKER_IDS[number]

export interface WaypointMarkerDefinition {
  id: WaypointMarkerId
  label: string
  glyph: string
  color: string
  gpxSymbol: string
  aliases?: readonly string[]
}

export const WAYPOINT_MARKERS: readonly WaypointMarkerDefinition[] = [
  { id: 'generic', label: 'Waypoint', glyph: '●', color: '#2563eb', gpxSymbol: 'Waypoint' },
  { id: 'fuel', label: 'Fuel', glyph: '⛽', color: '#dc2626', gpxSymbol: 'Gas Station', aliases: ['Fuel'] },
  { id: 'water', label: 'Drinking water', glyph: '💧', color: '#0284c7', gpxSymbol: 'Drinking Water', aliases: ['Water Source', 'Water'] },
  { id: 'camp', label: 'Campsite', glyph: '⛺', color: '#15803d', gpxSymbol: 'Campground', aliases: ['Camp Site', 'Camp'] },
  { id: 'food', label: 'Food', glyph: '🍴', color: '#c2410c', gpxSymbol: 'Restaurant', aliases: ['Food'] },
  { id: 'lodging', label: 'Lodging', glyph: '🛏', color: '#7c3aed', gpxSymbol: 'Lodging', aliases: ['Hotel'] },
  { id: 'parking', label: 'Parking', glyph: 'P', color: '#475569', gpxSymbol: 'Parking Area', aliases: ['Parking'] },
  { id: 'repair', label: 'Repair', glyph: '🔧', color: '#b45309', gpxSymbol: 'Repair', aliases: ['Service'] },
  { id: 'medical', label: 'Medical', glyph: '✚', color: '#e11d48', gpxSymbol: 'Medical Facility', aliases: ['Hospital', 'First Aid'] },
  { id: 'viewpoint', label: 'Viewpoint', glyph: '◉', color: '#0891b2', gpxSymbol: 'Scenic Area', aliases: ['Viewpoint'] },
  { id: 'hazard', label: 'Hazard', glyph: '!', color: '#ea580c', gpxSymbol: 'Danger Area', aliases: ['Hazard', 'Danger'] },
  { id: 'roadblock', label: 'Roadblock', glyph: '⛔', color: '#b91c1c', gpxSymbol: 'Road Block', aliases: ['Roadblock'] },
  { id: 'ferry', label: 'Ferry', glyph: '⛴', color: '#0369a1', gpxSymbol: 'Ferry' },
  { id: 'border', label: 'Border crossing', glyph: '⇄', color: '#4f46e5', gpxSymbol: 'Border Crossing', aliases: ['Border'] },
  { id: 'restroom', label: 'Restroom', glyph: 'WC', color: '#0f766e', gpxSymbol: 'Restroom', aliases: ['Toilet', 'Toilets'] },
  { id: 'information', label: 'Information', glyph: 'i', color: '#1d4ed8', gpxSymbol: 'Information' },
  { id: 'picnic', label: 'Picnic area', glyph: '🧺', color: '#65a30d', gpxSymbol: 'Picnic Area', aliases: ['Picnic'] },
]

const markersById = new Map(WAYPOINT_MARKERS.map(marker => [marker.id, marker]))
const markerIdsBySymbol = new Map<string, WaypointMarkerId>()

for (const marker of WAYPOINT_MARKERS) {
  for (const symbol of [marker.id, marker.label, marker.gpxSymbol, ...(marker.aliases ?? [])]) {
    markerIdsBySymbol.set(symbol.trim().toLowerCase(), marker.id)
  }
}

export function waypointMarkerDefinition(id: WaypointMarkerId): WaypointMarkerDefinition {
  return markersById.get(id)!
}

/** Empty symbols are ordinary generic waypoints; unknown imported symbols stay unknown. */
export function waypointMarkerIdFromSymbol(symbol?: string): WaypointMarkerId | undefined {
  if (!symbol?.trim()) return 'generic'
  return markerIdsBySymbol.get(symbol.trim().toLowerCase())
}
