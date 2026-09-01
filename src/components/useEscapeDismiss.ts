import { useLayoutEffect, useRef } from 'react'

export const ESCAPE_PRIORITY = {
  passive: 10,
  mode: 100,
  panel: 200,
  popover: 250,
  nested: 300,
  modal: 400,
} as const

interface EscapeDismissEntry {
  dismiss: () => void
  order: number
  priority: number
}

const entries = new Map<symbol, EscapeDismissEntry>()
let activationOrder = 0
let listening = false

function handleEscape(event: KeyboardEvent) {
  if (event.key !== 'Escape' || event.defaultPrevented || event.repeat || event.isComposing) return
  if (event.target instanceof HTMLSelectElement) return
  const popupClose = document.querySelector<HTMLElement>('.leaflet-popup-close-button')
  if (popupClose) {
    event.preventDefault()
    event.stopPropagation()
    popupClose.click()
    return
  }

  let top: EscapeDismissEntry | undefined
  for (const entry of entries.values()) {
    if (!top || entry.priority > top.priority || (entry.priority === top.priority && entry.order > top.order)) {
      top = entry
    }
  }
  if (!top) return

  event.preventDefault()
  event.stopPropagation()
  top.dismiss()
}

function updateListener() {
  if (entries.size > 0 && !listening) {
    window.addEventListener('keydown', handleEscape, true)
    listening = true
  } else if (entries.size === 0 && listening) {
    window.removeEventListener('keydown', handleEscape, true)
    listening = false
  }
}

export function useEscapeDismiss(active: boolean, onDismiss: () => void, priority: number = ESCAPE_PRIORITY.panel) {
  const dismissRef = useRef(onDismiss)
  const keyRef = useRef(Symbol('escape-dismiss'))
  dismissRef.current = onDismiss

  useLayoutEffect(() => {
    if (!active) return
    const key = keyRef.current
    entries.set(key, {
      dismiss: () => dismissRef.current(),
      order: ++activationOrder,
      priority,
    })
    updateListener()
    return () => {
      entries.delete(key)
      updateListener()
    }
  }, [active, priority])
}
