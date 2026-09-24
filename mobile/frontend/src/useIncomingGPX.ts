import { useCallback, useEffect, useRef, useState } from 'react'
import { request } from './model'

interface IncomingFile { id: string; filename: string }
interface Options {
  enabled: boolean
  blocked: boolean
  dirty: boolean
  onImport: (content: string, filename: string) => void
  onError: (message: string) => void
}

async function acknowledge(id: string, signal?: AbortSignal) {
  const response = await fetch(`/mobile/incoming/${encodeURIComponent(id)}`, {
    method: 'DELETE', headers: { 'X-GPX-Editor': '1' }, signal,
  })
  if (!response.ok) throw new Error('Could not dismiss the shared file. Try again.')
}

/** The native inbox survives startup/reloads; reading is separate from acknowledgement. */
export function useIncomingGPX(options: Options) {
  const latest = useRef(options)
  latest.current = options
  const [pending, setPending] = useState<IncomingFile | null>(null)
  const [error, setError] = useState('')
  const [working, setWorking] = useState(false)
  const inFlight = useRef(false)
  const readController = useRef<AbortController | null>(null)
  // If acknowledgement fails after opening, retry cleanup without reimporting.
  const handled = useRef(new Set<string>())

  useEffect(() => {
    if (!options.enabled) return
    const controller = new AbortController()
    let polling = false
    const poll = async () => {
      if (polling || controller.signal.aborted) return
      polling = true
      try {
        const items = await request<IncomingFile[]>('/mobile/incoming', { signal: controller.signal })
        if (controller.signal.aborted || !Array.isArray(items)) return
        for (const id of handled.current) {
          if (!items.some(item => item.id === id)) handled.current.delete(id)
          else await acknowledge(id, controller.signal).catch(() => {})
        }
        const next = items.find(item => !handled.current.has(item.id))
        if (!controller.signal.aborted && next) setPending(previous => previous ?? next)
      } catch { /* Optional native service; ordinary editing remains available. */ }
      finally { polling = false }
    }
    void poll()
    const timer = setInterval(() => void poll(), 1500)
    const focus = () => void poll()
    window.addEventListener('focus', focus)
    return () => {
      controller.abort()
      readController.current?.abort()
      clearInterval(timer)
      window.removeEventListener('focus', focus)
    }
  }, [options.enabled])

  const open = useCallback(async (item: IncomingFile, confirmed = false) => {
    if (inFlight.current || handled.current.has(item.id)) return
    inFlight.current = true
    setWorking(true)
    setError('')
    const controller = new AbortController()
    readController.current = controller
    try {
      const file = await request<{ filename: string; content: string }>(
        `/mobile/incoming/${encodeURIComponent(item.id)}`, { signal: controller.signal },
      )
      if (controller.signal.aborted) return
      // The user may have started editing while the file was being read.
      if (!confirmed && (latest.current.dirty || latest.current.blocked)) return
      latest.current.onImport(file.content, file.filename)
      handled.current.add(item.id)
      setPending(null)
      await acknowledge(item.id, controller.signal).catch(() => {})
    } catch (reason) {
      if (!controller.signal.aborted) {
        const message = (reason as Error).message
        setError(message)
        latest.current.onError(message)
      }
    } finally {
      inFlight.current = false
      setWorking(false)
    }
  }, [])

  useEffect(() => {
    if (options.enabled && pending && !options.blocked && !options.dirty && !working && !error) {
      void open(pending)
    }
  }, [options.enabled, options.blocked, options.dirty, pending, working, error, open])

  const dismiss = async () => {
    if (!pending || inFlight.current) return
    try {
      await acknowledge(pending.id)
      handled.current.add(pending.id)
      setPending(null)
      setError('')
    } catch (reason) { latest.current.onError((reason as Error).message) }
  }
  return { pending, working, error, open: () => pending ? open(pending, true) : Promise.resolve(), dismiss }
}
