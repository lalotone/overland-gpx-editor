import { useCallback, useEffect, useMemo } from 'react'
import {
  MapContainer,
  Marker,
  Popup,
  Polyline,
  CircleMarker,
  useMap,
  useMapEvents,
} from 'react-leaflet'
import L from 'leaflet'
import { MapTiles } from '../../../src/components/MapLayers'
import { ColoredTrack } from '../../../src/components/ColoredTrack'
import { runtimeTerrainLayers, runtimeHillshadeLayer } from '../../../src/lib/terrain'
import type { RuntimeConfig } from '../../../src/lib/offline'
import type { Coordinate, GpxWaypoint } from '../../../src/lib/types'
import type { Poi, BoundingBox } from '../../../src/lib/poi'
import type { SurfaceClass } from '../../../src/lib/surface'
import { useTrackStats } from './TrackSummary'

function Events({
  onView,
  onMap,
  onMovePoint,
  points,
}: {
  onView: (center: Coordinate, bounds: BoundingBox) => void
  onMap: (map: L.Map) => void
  onMovePoint: (index: number, point: Coordinate) => void
  points: Coordinate[]
}) {
  const map = useMapEvents({ moveend: report, zoomend: report })
  function report() {
    const p = map.getCenter(),
      b = map.getBounds()
    onView(
      { lat: p.lat, lon: p.lng },
      { south: b.getSouth(), west: b.getWest(), north: b.getNorth(), east: b.getEast() },
    )
  }
  useEffect(() => {
    onMap(map)
    report()
    const observer = new ResizeObserver(() => map.invalidateSize())
    observer.observe(map.getContainer())
    return () => observer.disconnect()
  }, [map, onMap])
  return (
    <>
      {points.map((p, index) => (
        <Marker
          key={index}
          position={[p.lat, p.lon]}
          draggable
          icon={L.divIcon({
            className: 'route-point',
            html: String(index + 1),
            iconSize: [32, 32],
            iconAnchor: [16, 16],
          })}
          eventHandlers={{
            dragend: (event) => {
              const point = (event.target as L.Marker).getLatLng()
              onMovePoint(index, { lat: point.lat, lon: point.lng })
            },
          }}
        />
      ))}
    </>
  )
}

function CreditsPosition() {
  const map = useMap()
  useEffect(() => {
    map.attributionControl.setPrefix(false)
  }, [map])
  return null
}

export default function MobileMap({
  runtime,
  coordinates,
  points,
  pins,
  pois,
  surfaces,
  layer,
  relief,
  onView,
  onMap,
  onMovePoint,
  onError,
}: {
  runtime: RuntimeConfig
  coordinates: Coordinate[]
  points: Coordinate[]
  pins: GpxWaypoint[]
  pois: Poi[]
  surfaces?: SurfaceClass[]
  layer: string
  relief: boolean
  onView: (center: Coordinate, bounds: BoundingBox) => void
  onMap: (map: L.Map) => void
  onMovePoint: (index: number, point: Coordinate) => void
  onError: (message: string) => void
}) {
  const layers = useMemo(() => runtimeTerrainLayers(runtime), [runtime])
  const vectorStatus = useCallback(
    (issue: { message: string } | null) => {
      if (issue) onError(issue.message)
    },
    [onError],
  )
  const { cumKm, elevations, stats } = useTrackStats(coordinates)
  return (
    <MapContainer
      center={[41.65, -0.88]}
      zoom={11}
      zoomControl={false}
      preferCanvas
      className="mobile-map"
    >
      <CreditsPosition />
      <MapTiles
        layers={layers}
        baseLayerId={layer}
        hillshade={relief}
        hillshadeOpacity={0.25}
        hillshadeLayer={runtimeHillshadeLayer(runtime)}
        onVectorStatus={vectorStatus}
      />
      <Events points={points} onMovePoint={onMovePoint} onView={onView} onMap={onMap} />
      {coordinates.length > 1 && (
        <>
          <Polyline
            positions={coordinates.map((p) => [p.lat, p.lon])}
            pathOptions={{ color: '#fff', weight: 8, opacity: 0.9 }}
          />
          <ColoredTrack
            coordinates={coordinates}
            cumKm={cumKm}
            elevations={elevations}
            colorMode={surfaces ? 'surface' : 'altitude'}
            elevMin={stats.min}
            elevMax={stats.max}
            surfaces={surfaces}
            weight={5}
          />
        </>
      )}
      {coordinates.length === 0 && points.length > 1 && (
        <Polyline
          positions={points.map((p) => [p.lat, p.lon])}
          pathOptions={{ color: '#a68c76', weight: 2, dashArray: '5 8' }}
        />
      )}
      {pins.map((p, i) => (
        <CircleMarker
          key={i}
          center={[p.lat, p.lon]}
          radius={7}
          pathOptions={{ color: '#fff', fillColor: '#bd794d', fillOpacity: 1 }}
        >
          <Popup>
            <strong>{p.name || 'Saved place'}</strong>
            {p.desc && <p>{p.desc}</p>}
          </Popup>
        </CircleMarker>
      ))}
      {pois.map((p) => (
        <CircleMarker
          key={p.id}
          center={[p.lat, p.lon]}
          radius={8}
          pathOptions={{
            color: '#fff',
            fillColor: p.kind === 'fuel' ? '#b87348' : p.kind === 'water' ? '#5084a1' : '#5f7b58',
            fillOpacity: 1,
          }}
        >
          <Popup>
            <strong>{p.name || p.kind}</strong>
            {p.detail?.lines.map((line) => (
              <p key={line.label}>
                {line.label}: {line.value}
              </p>
            ))}
            {p.detail?.source && <small>{p.detail.source}</small>}
          </Popup>
        </CircleMarker>
      ))}
    </MapContainer>
  )
}
