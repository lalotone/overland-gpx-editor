import React from 'react'
import ReactDOM from 'react-dom/client'
import L from 'leaflet'
import { setWorkerUrl } from 'maplibre-gl'
import maplibreWorkerUrl from 'maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url'
import App from './App.tsx'
// Bundled rather than pulled from a CDN so the binary carries everything it
// needs — the map still styles correctly on a machine with no internet.
import 'leaflet/dist/leaflet.css'
import 'maplibre-gl/dist/maplibre-gl.css'
import markerIcon from 'leaflet/dist/images/marker-icon.png'
import markerIcon2x from 'leaflet/dist/images/marker-icon-2x.png'
import markerShadow from 'leaflet/dist/images/marker-shadow.png'
import './index.css'

// MapLibre 6 ships its vector-tile decoder as a separate ESM worker. Let Vite
// bundle it so the production binary serves a self-contained, same-origin URL.
setWorkerUrl(maplibreWorkerUrl)

// Leaflet finds its default marker images by parsing the URL out of the
// stylesheet, which cannot work once the bundler has inlined that URL. Point
// it at the bundled images instead, or the draggable waypoint markers on the
// creation screen render as broken images.
L.Icon.Default.mergeOptions({
  iconUrl: markerIcon,
  iconRetinaUrl: markerIcon2x,
  shadowUrl: markerShadow,
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
