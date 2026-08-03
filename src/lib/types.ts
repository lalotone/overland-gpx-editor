/* Shared domain types. */

export interface Coordinate {
  lat: number
  lon: number
  elevation?: number
  /** ISO-8601 timestamp from <time>, when the source recorded one. */
  time?: string
}

export interface GpxWaypoint {
  lat: number
  lon: number
  name?: string
  desc?: string
  /** Garmin symbol name, e.g. "Gas Station". Preserved on round-trip. */
  sym?: string
  type?: string
  elevation?: number
}

export interface Track {
  name: string
  filename: string
  coordinates: Coordinate[]
  /** Parallel to `coordinates`; null where the source had no <ele>. */
  elevations: (number | null)[]
  /** File-level <wpt> elements. Shared by every track parsed from one file. */
  waypoints: GpxWaypoint[]
  /** metadata/<time>, falling back to the first trackpoint's timestamp. */
  time?: string
  creator?: string
  /** True when this came from a <rte> (planned route) rather than a <trk>. */
  isRoute?: boolean
}

export interface ElevationStats {
  min: number
  max: number
  gain: number
  loss: number
}

export interface TimeStats {
  totalSeconds: number
  movingSeconds: number
  avgSpeedKmh: number
  movingSpeedKmh: number
  maxSpeedKmh: number
}
