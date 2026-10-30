/*
 * The Android window around the WebView. Android 15+ draws the app edge to
 * edge and the WebView reports zero safe-area insets, so the native
 * OverlandChrome interface supplies the system bar sizes, which become
 * --safe-* CSS variables (mobile.css falls back to env() elsewhere). It also
 * picks dark or light bar icons to suit the theme. In a browser or the
 * preview host there is no native interface and nothing changes.
 */

interface NativeChrome {
  insets(): string
  setDarkChrome(dark: boolean): void
}

function native(): NativeChrome | undefined {
  return (window as unknown as { OverlandChrome?: NativeChrome }).OverlandChrome
}

function applyInsets(): void {
  const chrome = native()
  if (!chrome) return
  try {
    const insets = JSON.parse(chrome.insets()) as Record<'top' | 'bottom' | 'left' | 'right', number>
    const style = document.documentElement.style
    for (const side of ['top', 'bottom', 'left', 'right'] as const) {
      const value = Number(insets[side])
      if (Number.isFinite(value) && value >= 0) style.setProperty(`--safe-${side}`, `${value}px`)
    }
  } catch {
    /* Keep the env() fallbacks. */
  }
}

/** Reads the insets now and again whenever the window changes shape. */
export function watchInsets(): void {
  if (!native()) return
  applyInsets()
  // The window may not have its insets yet on the very first frame.
  for (const delay of [50, 250, 1000]) window.setTimeout(applyInsets, delay)
  window.addEventListener('resize', applyInsets)
  window.addEventListener('orientationchange', () => window.setTimeout(applyInsets, 150))
}

export function setDarkChrome(dark: boolean): void {
  try {
    native()?.setDarkChrome(dark)
  } catch {
    /* Cosmetic only. */
  }
}
