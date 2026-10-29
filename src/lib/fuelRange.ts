/*
 * Tank range against the fuel stations along a track.
 *
 * The rider gives a full-tank range and a reserve they do not want to dip
 * into. Starting full at km 0 (stated in the UI, because it is an assumption
 * the data cannot check), every stretch between one fuel opportunity and the
 * next is compared with both:
 *
 * - longer than the usable range (range minus reserve): you arrive on reserve;
 * - longer than the full range: you run dry before the next pump.
 *
 * A station counts as "on the route" at the track point it passes closest to,
 * inside a corridor. The detour to reach it is not added, and opening hours,
 * card-only pumps and fuel grade are not known — the numbers are a floor.
 */

import { haversineDistance } from './geo'
import type { Coordinate } from './types'

export interface FuelRangeSettings {
  /** Distance on a full tank, km. */
  rangeKm: number
  /** Share of the tank kept in reserve, 0–90 %. */
  reservePct: number
}

export const DEFAULT_FUEL_RANGE: FuelRangeSettings = { rangeKm: 250, reservePct: 15 }

export interface FuelStation {
  id: string
  lat: number
  lon: number
  name?: string
}

export interface FuelStop {
  km: number
  station: FuelStation
  /** Distance from the track at the closest approach, metres. */
  offsetM: number
}

export type FuelLegStatus = 'ok' | 'reserve' | 'dry'

export interface FuelLeg {
  fromKm: number
  toKm: number
  /** Null for the start of the track. */
  from: FuelStop | null
  /** Null for the end of the track. */
  to: FuelStop | null
  status: FuelLegStatus
  /** Where the reserve starts (`reserve`) or the tank empties (`dry`). */
  atKm?: number
}

export interface FuelRangePlan {
  usableKm: number
  stops: FuelStop[]
  /** Legs that need attention, in track order. */
  warnings: FuelLeg[]
  longestLegKm: number
}

export function normalizeFuelRange(settings: Partial<FuelRangeSettings>): FuelRangeSettings {
  const rangeKm = Number(settings.rangeKm)
  const reservePct = Number(settings.reservePct)
  return {
    rangeKm: Number.isFinite(rangeKm) && rangeKm >= 10 ? Math.min(rangeKm, 5000) : DEFAULT_FUEL_RANGE.rangeKm,
    reservePct: Number.isFinite(reservePct) && reservePct >= 0 ? Math.min(reservePct, 90) : DEFAULT_FUEL_RANGE.reservePct,
  }
}

/**
 * Stations within `corridorM` of the track, each placed at the point the
 * track passes closest to it. A station the track passes twice — an out and
 * back — is listed once per pass.
 */
export function fuelStopsAlong(
  coords: Coordinate[],
  cumKm: number[],
  stations: FuelStation[],
  corridorM: number,
): FuelStop[] {
  if (coords.length === 0 || stations.length === 0) return []
  // Cheap degree box first; haversine only for points that can be in range.
  const latPad = corridorM / 111_000
  const stops: FuelStop[] = []
  for (const station of stations) {
    const lonPad = latPad / Math.max(0.05, Math.cos((station.lat * Math.PI) / 180))
    let best: FuelStop | null = null
    for (let i = 0; i < coords.length; i++) {
      const point = coords[i]
      if (Math.abs(point.lat - station.lat) > latPad || Math.abs(point.lon - station.lon) > lonPad) {
        if (best) { stops.push(best); best = null }
        continue
      }
      const offsetM = haversineDistance(point, station as Coordinate)
      if (offsetM > corridorM) {
        if (best) { stops.push(best); best = null }
        continue
      }
      if (!best || offsetM < best.offsetM) best = { km: cumKm[i], station, offsetM }
    }
    if (best) stops.push(best)
  }
  return stops.sort((a, b) => a.km - b.km)
}

export function planFuelRange(totalKm: number, stops: FuelStop[], input: FuelRangeSettings): FuelRangePlan {
  const settings = normalizeFuelRange(input)
  const usableKm = settings.rangeKm * (1 - settings.reservePct / 100)
  const ordered = [...stops].sort((a, b) => a.km - b.km)
  const points: (FuelStop | null)[] = [null, ...ordered, null]
  const kmOf = (index: number) => {
    const stop = points[index]
    if (stop) return stop.km
    return index === 0 ? 0 : totalKm
  }
  const warnings: FuelLeg[] = []
  let longestLegKm = 0
  for (let i = 1; i < points.length; i++) {
    const fromKm = kmOf(i - 1)
    const toKm = kmOf(i)
    const leg = toKm - fromKm
    longestLegKm = Math.max(longestLegKm, leg)
    if (leg <= usableKm) continue
    const dry = leg > settings.rangeKm
    warnings.push({
      fromKm,
      toKm,
      from: points[i - 1],
      to: points[i],
      status: dry ? 'dry' : 'reserve',
      atKm: fromKm + (dry ? settings.rangeKm : usableKm),
    })
  }
  return { usableKm, stops: ordered, warnings, longestLegKm }
}
