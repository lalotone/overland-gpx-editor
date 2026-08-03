import React from 'react'
import ReactDOM from 'react-dom/client'
import L from 'leaflet'
import App from './App.tsx'
// Bundled rather than pulled from a CDN so the binary carries everything it
// needs — the map still styles correctly on a machine with no internet.
import 'leaflet/dist/leaflet.css'
import markerIcon from 'leaflet/dist/images/marker-icon.png'
import markerIcon2x from 'leaflet/dist/images/marker-icon-2x.png'
import markerShadow from 'leaflet/dist/images/marker-shadow.png'
import './index.css'

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