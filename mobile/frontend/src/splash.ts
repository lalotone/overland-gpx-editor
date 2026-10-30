/*
 * The launch splash lives in index.html so it paints before any JavaScript.
 * The app only reports progress and says when it is ready; this module owns
 * the timing so the splash never flashes and never outstays its welcome.
 */

/** Short launches still show the mark rather than a one-frame flicker. */
const MIN_VISIBLE_MS = 600
/** After this the app's own startup notice explains a slow first launch. */
export const SPLASH_MAX_MS = 8000

let hidden = false
let gone = false
const listeners = new Set<() => void>()

/** Runs once the splash starts fading (at once if it already has). */
export function whenSplashHidden(callback: () => void): () => void {
  if (gone) {
    callback()
    return () => {}
  }
  listeners.add(callback)
  return () => listeners.delete(callback)
}

function element(): HTMLElement | null {
  return document.getElementById('splash')
}

export function setSplashStatus(text: string): void {
  const status = document.getElementById('splash-status')
  if (status && status.textContent !== text) status.textContent = text
}

export function hideSplash(): void {
  if (hidden) return
  hidden = true
  const splash = element()
  if (!splash) {
    gone = true
    listeners.forEach((callback) => callback())
    listeners.clear()
    return
  }
  const wait = Math.max(0, MIN_VISIBLE_MS - performance.now())
  window.setTimeout(() => {
    gone = true
    listeners.forEach((callback) => callback())
    listeners.clear()
    splash.classList.add('done')
    splash.setAttribute('aria-hidden', 'true')
    // Gone from the tree once faded: no stray layer over the map.
    window.setTimeout(() => splash.remove(), 400)
  }, wait)
}
