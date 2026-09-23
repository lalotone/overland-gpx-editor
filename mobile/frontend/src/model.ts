import type { Coordinate, Track } from '../../../src/lib/types'
import type { RoutingProfile } from '../../../src/lib/routing'
import { buildGPX } from '../../../src/lib/gpx'

export type Tab = 'explore' | 'plan' | 'library' | 'offline'
export interface Plan {
  name: string
  points: Coordinate[]
  profile: RoutingProfile
  permit: boolean
}
export interface Document {
  track: Track
  libraryFilename: string | null
  dirty: boolean
}
export interface Draft {
  version: 1
  plan: Plan
  document: Document | null
  tab: Tab
}
export const emptyPlan = (): Plan => ({
  name: 'New route',
  points: [],
  profile: 'mixed',
  permit: false,
})
export const trackGPX = (track: Track) =>
  buildGPX({
    name: track.name,
    coordinates: track.coordinates,
    waypoints: track.waypoints,
    time: track.time,
  })

export async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, init)
  if (!response.ok) {
    const text = await response.text()
    let detail = text
    try {
      const body = JSON.parse(text)
      detail = body.detail ?? body.error ?? text
    } catch {
      /* Plain HTTP errors are valid too. */
    }
    throw new Error(detail.slice(0, 350) || `Request failed (${response.status})`)
  }
  return response.json() as Promise<T>
}
export const jsonBody = (body: unknown, method = 'POST'): RequestInit => ({
  method,
  headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
  body: JSON.stringify(body),
})

function points(value: unknown): value is Coordinate[] {
  return (
    Array.isArray(value) &&
    value.length <= 200000 &&
    value.every(
      (p) =>
        p &&
        Number.isFinite(p.lat) &&
        Math.abs(p.lat) <= 90 &&
        Number.isFinite(p.lon) &&
        Math.abs(p.lon) <= 180,
    )
  )
}
export function decodeDraft(raw: unknown): Draft | null {
  if (!raw || typeof raw !== 'object') return null
  const value = raw as Draft
  if (
    value.version !== 1 ||
    !value.plan ||
    !points(value.plan.points) ||
    value.plan.points.length > 100 ||
    !['road', 'mixed', 'trail', 'enduro'].includes(value.plan.profile) ||
    typeof value.plan.name !== 'string' ||
    typeof value.plan.permit !== 'boolean'
  )
    return null
  if (!['explore', 'plan', 'library', 'offline'].includes(value.tab)) return null
  if (
    value.document &&
    (!value.document.track ||
      !points(value.document.track.coordinates) ||
      !Array.isArray(value.document.track.waypoints) ||
      typeof value.document.track.name !== 'string')
  )
    return null
  return value
}
