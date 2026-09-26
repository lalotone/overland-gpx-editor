import { useEffect, useState } from 'react'

export interface PasskeySession {
  username: string
}

let sessionScriptLoaded = false

/**
 * Detects `overland serve --auth`. With it on, the server only hands this page
 * to a signed-in browser, so the probe exists to name the account and offer
 * sign-out, not to gate anything. Without it `/auth/api/me` falls through to
 * the SPA shell, which is not JSON, and the hook stays null.
 *
 * Passkeys and the SameSite=Strict session cookie are bound to the origin the
 * page came from, so a bundle pointed at a remote backend never probes.
 */
export function usePasskeySession(apiBase: string): PasskeySession | null {
  const [session, setSession] = useState<PasskeySession | null>(null)

  useEffect(() => {
    if (apiBase) return
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch('/auth/api/me', {
          headers: { Accept: 'application/json' },
          signal: controller.signal,
        })
        if (!response.ok || !response.headers.get('Content-Type')?.includes('application/json')) return
        const body: unknown = await response.json()
        const username = (body as { username?: unknown } | null)?.username
        if (typeof username !== 'string') return
        loadSessionScript()
        setSession({ username })
      } catch {
        // No backend, or no auth: the app works the same either way.
      }
    })()
    return () => controller.abort()
  }, [apiBase])

  return session
}

// session.js sends the browser back to sign-in when any same-origin request
// gets a 401, which is how a revoked or expired session surfaces.
function loadSessionScript() {
  if (sessionScriptLoaded) return
  sessionScriptLoaded = true
  const script = document.createElement('script')
  script.src = '/auth/session.js'
  script.defer = true
  document.head.appendChild(script)
}

export async function signOut(): Promise<void> {
  try {
    await fetch('/auth/api/logout', { method: 'POST' })
  } finally {
    location.assign('/')
  }
}
