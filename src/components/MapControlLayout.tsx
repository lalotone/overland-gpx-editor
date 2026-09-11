import { useEffect } from 'react'
import { useMap } from 'react-leaflet'

/** Keep floating tools clear of readouts and wrapping POI/attribution strips. */
export default function MapControlLayout() {
  const map = useMap()
  useEffect(() => {
    const container = map.getContainer()
    const parent = container.parentElement ?? container
    const selectors = '.map-cursor-readout, .fuel-legend, .creation-poi-strip, .leaflet-control-attribution'
    const reposition = () => {
      let routingBottom = 28
      const mapBounds = container.getBoundingClientRect()
      parent.querySelectorAll('.map-cursor-readout, .fuel-legend').forEach(overlay => {
        const bounds = overlay.getBoundingClientRect()
        if (bounds.height && bounds.width) routingBottom = Math.max(routingBottom, mapBounds.bottom - bounds.top + 8)
      })
      const strip = parent.querySelector('.creation-poi-strip')?.getBoundingClientRect()
      const attribution = container.querySelector('.leaflet-control-attribution')?.getBoundingClientRect()
      const zoomBottom = strip?.height ? Math.max(10, mapBounds.bottom - strip.top + 8 - (attribution?.height ?? 0)) : 10
      if (strip?.height && mapBounds.width < 600) routingBottom = Math.max(routingBottom, mapBounds.bottom - strip.top + 8)
      container.style.setProperty('--routing-control-bottom', `${routingBottom}px`)
      container.style.setProperty('--routing-panel-max-height', `${Math.max(100, mapBounds.height - routingBottom - 100)}px`)
      container.style.setProperty('--zoom-control-gap', `${zoomBottom}px`)
    }
    const resize = new ResizeObserver(reposition)
    const watch = () => {
      resize.disconnect()
      resize.observe(container)
      parent.querySelectorAll(selectors).forEach(overlay => resize.observe(overlay))
      reposition()
    }
    const mutations = new MutationObserver(watch)
    mutations.observe(parent, { childList: true, subtree: true })
    watch()
    return () => { resize.disconnect(); mutations.disconnect() }
  }, [map])
  return null
}
