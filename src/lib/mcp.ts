import type { Coordinate, GpxWaypoint } from './types'
import type { PoiKind } from './poi'
import type { RoutingProfile } from './routing'
import {
  WAYPOINT_MARKER_IDS,
  waypointMarkerDefinition,
} from './waypointMarkers'
import type { WaypointMarkerId } from './waypointMarkers'

type EditMode = 'replace' | 'append'
export type McpMapMode = 'planner' | 'editor' | 'explore'

export interface McpMapMarker extends Coordinate {
  marker: WaypointMarkerId
  name?: string
  desc?: string
}

export type McpCommand =
  | { id: string; name: 'plan_route'; arguments: { points: Coordinate[]; mode: EditMode; profile?: RoutingProfile; fitView: boolean } }
  | { id: string; name: 'draw_track'; arguments: { points: Coordinate[]; mode: EditMode; fitView: boolean } }
  | { id: string; name: 'set_waypoints'; arguments: { target: 'planner' | 'track'; mode: EditMode; waypoints: GpxWaypoint[] } }
  | { id: string; name: 'draw_map_track'; arguments: { points: Coordinate[]; mode: EditMode; fitView: boolean } }
  | { id: string; name: 'set_map_markers'; arguments: { markers: McpMapMarker[]; mode: EditMode; fitView: boolean } }
  | { id: string; name: 'switch_mode'; arguments: { mode: McpMapMode } }
  | { id: string; name: 'set_map_view'; arguments: { lat: number; lon: number; zoom?: number } }
  | { id: string; name: 'open_track'; arguments: { filename: string } }
  | { id: string; name: 'select_track'; arguments: { index: number } }
  | { id: string; name: 'load_pois'; arguments: { kinds: PoiKind[]; scope: 'current_view' | 'current_track' } }

export function parseMcpCommand(value: unknown): McpCommand {
  const envelope = record(value, 'command')
  const id = text(envelope.id, 'command id', 128)
  const name = text(envelope.name, 'command name', 100)
  const args = record(envelope.arguments, 'command arguments')

  switch (name) {
    case 'plan_route':
      return {
        id,
        name,
        arguments: {
          points: points(args.points, 100, true),
          mode: editMode(args.mode),
          profile: optionalEnum(args.profile, ['road', 'mixed', 'trail'], 'profile'),
          fitView: optionalBoolean(args.fitView, true),
        },
      }
    case 'draw_track':
      return {
        id,
        name,
        arguments: {
          points: points(args.points, 6000, false),
          mode: editMode(args.mode),
          fitView: optionalBoolean(args.fitView, true),
        },
      }
    case 'set_waypoints': {
      const target = enumValue(args.target, ['planner', 'track'], 'target')
      const rawWaypoints = array(args.waypoints, 'waypoints')
      if (rawWaypoints.length > 1000) throw new Error('waypoints must contain at most 1000 items')
      return {
        id,
        name,
        arguments: {
          target,
          mode: editMode(args.mode),
          waypoints: rawWaypoints.map((item, index) => waypoint(item, index)),
        },
      }
    }
    case 'draw_map_track':
      return {
        id,
        name,
        arguments: {
          points: points(args.points, 6000, true),
          mode: editMode(args.mode),
          fitView: optionalBoolean(args.fitView, true),
        },
      }
    case 'set_map_markers': {
      const rawMarkers = array(args.markers, 'markers')
      if (rawMarkers.length > 1000) throw new Error('markers must contain at most 1000 items')
      return {
        id,
        name,
        arguments: {
          markers: rawMarkers.map((item, index) => mapMarker(item, index)),
          mode: editMode(args.mode),
          fitView: optionalBoolean(args.fitView, true),
        },
      }
    }
    case 'switch_mode':
      return { id, name, arguments: { mode: enumValue(args.mode, ['planner', 'editor', 'explore'], 'mode') } }
    case 'set_map_view': {
      const lat = finiteNumber(args.lat, 'latitude')
      const lon = finiteNumber(args.lon, 'longitude')
      validateLatLon(lat, lon)
      const zoom = args.zoom === undefined ? undefined : finiteNumber(args.zoom, 'zoom')
      if (zoom !== undefined && (zoom < 1 || zoom > 20)) throw new Error('zoom must be between 1 and 20')
      return { id, name, arguments: { lat, lon, zoom } }
    }
    case 'open_track': {
      const filename = text(args.filename, 'filename', 255)
      if (filename.includes('\0') || !/^(?!\.)[^/\\]+\.gpx$/i.test(filename)) {
        throw new Error('filename must be a bare non-hidden .gpx filename')
      }
      return { id, name, arguments: { filename } }
    }
    case 'select_track': {
      const index = finiteNumber(args.index, 'index')
      if (!Number.isInteger(index) || index < 0) throw new Error('index must be a non-negative integer')
      return { id, name, arguments: { index } }
    }
    case 'load_pois': {
      const rawKinds = array(args.kinds, 'kinds')
      const kinds = rawKinds.map(kind => enumValue(kind, ['fuel', 'water', 'camp'], 'POI kind'))
      if (kinds.length === 0 || kinds.length > 3 || new Set(kinds).size !== kinds.length) {
        throw new Error('kinds must contain 1 to 3 unique POI kinds')
      }
      return {
        id,
        name,
        arguments: { kinds, scope: enumValue(args.scope, ['current_view', 'current_track'], 'scope') },
      }
    }
    default:
      throw new Error(`unsupported MCP command: ${name}`)
  }
}

function points(value: unknown, limit: number, allowEmpty: boolean): Coordinate[] {
  const values = array(value, 'points')
  if ((!allowEmpty && values.length === 0) || values.length > limit) {
    throw new Error(`points must contain ${allowEmpty ? 'at most' : '1 to'} ${limit} coordinates`)
  }
  return values.map((item, index) => coordinate(item, `point ${index}`))
}

function coordinate(value: unknown, label: string): Coordinate {
  const item = record(value, label)
  const lat = finiteNumber(item.lat, `${label} latitude`)
  const lon = finiteNumber(item.lon, `${label} longitude`)
  validateLatLon(lat, lon)
  const elevation = item.elevation === undefined ? undefined : finiteNumber(item.elevation, `${label} elevation`)
  return { lat, lon, elevation }
}

function waypoint(value: unknown, index: number): GpxWaypoint {
  const item = record(value, `waypoint ${index}`)
  const coord = coordinate(item, `waypoint ${index}`)
  const marker = optionalEnum(item.marker, WAYPOINT_MARKER_IDS, `waypoint ${index} marker`)
  const symbol = optionalText(item.sym, `waypoint ${index} symbol`, 100)
  if (marker && symbol !== undefined) throw new Error(`waypoint ${index} cannot set both marker and sym`)
  return {
    ...coord,
    name: optionalText(item.name, `waypoint ${index} name`, 200),
    desc: optionalText(item.desc, `waypoint ${index} description`, 2000),
    sym: marker ? waypointMarkerDefinition(marker).gpxSymbol : symbol,
    type: optionalText(item.type, `waypoint ${index} type`, 100),
  }
}

function mapMarker(value: unknown, index: number): McpMapMarker {
  const item = record(value, `marker ${index}`)
  return {
    ...coordinate(item, `marker ${index}`),
    marker: enumValue(item.marker, WAYPOINT_MARKER_IDS, `marker ${index} type`),
    name: optionalText(item.name, `marker ${index} name`, 200),
    desc: optionalText(item.desc, `marker ${index} description`, 2000),
  }
}

function validateLatLon(lat: number, lon: number) {
  if (lat < -90 || lat > 90 || lon < -180 || lon > 180) {
    throw new Error('latitude or longitude is out of range')
  }
}

function editMode(value: unknown): EditMode {
  return value === undefined ? 'replace' : enumValue(value, ['replace', 'append'], 'mode')
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error(`${label} must be an object`)
  return value as Record<string, unknown>
}

function array(value: unknown, label: string): unknown[] {
  if (!Array.isArray(value)) throw new Error(`${label} must be an array`)
  return value
}

function finiteNumber(value: unknown, label: string): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new Error(`${label} must be a finite number`)
  return value
}

function text(value: unknown, label: string, maxLength: number): string {
  if (typeof value !== 'string' || value.length === 0 || [...value].length > maxLength) {
    throw new Error(`${label} must be a non-empty string no longer than ${maxLength} characters`)
  }
  return value
}

function optionalText(value: unknown, label: string, maxLength: number): string | undefined {
  if (value === undefined) return undefined
  if (typeof value !== 'string' || [...value].length > maxLength) throw new Error(`${label} is too long`)
  return value
}

function optionalBoolean(value: unknown, fallback: boolean): boolean {
  if (value === undefined) return fallback
  if (typeof value !== 'boolean') throw new Error('boolean argument has the wrong type')
  return value
}

function enumValue<const T extends string>(value: unknown, options: readonly T[], label: string): T {
  if (typeof value !== 'string' || !options.includes(value as T)) {
    throw new Error(`${label} must be one of ${options.join(', ')}`)
  }
  return value as T
}

function optionalEnum<const T extends string>(value: unknown, options: readonly T[], label: string): T | undefined {
  return value === undefined ? undefined : enumValue(value, options, label)
}
