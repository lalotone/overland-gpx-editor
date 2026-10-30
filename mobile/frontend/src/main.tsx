import React from 'react'
import ReactDOM from 'react-dom/client'
import { setWorkerUrl } from 'maplibre-gl'
import maplibreWorkerUrl from 'maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url'
import 'leaflet/dist/leaflet.css'
import 'maplibre-gl/dist/maplibre-gl.css'
import App from './App'
import { watchInsets } from './chrome'
import './mobile.css'

setWorkerUrl(maplibreWorkerUrl)
watchInsets()
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
