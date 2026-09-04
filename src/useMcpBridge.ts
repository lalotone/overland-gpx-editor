import { useEffect, useRef } from 'react'

import { parseMcpCommand } from './lib/mcp'
import type { McpCommand } from './lib/mcp'

const HEARTBEAT_INTERVAL_MS = 1000
/** Backoff before re-announcing a view the broker has forgotten. */
const STREAM_RETRY_MS = 1000
const RESULT_RETRY_MS = 250
const MAX_RESULT_ATTEMPTS = 10
const PUBLISH_RETRY_MS = 150
const MAX_PUBLISH_ATTEMPTS = 4
/** Total budget for the publish that precedes an acknowledgement. */
const PUBLISH_DEADLINE_MS = 1500
/** Bounds the redelivery guard; the broker queues far fewer than this. */
const MAX_TRACKED_COMMANDS = 128

/**
 * Connect the active page to the optional loopback MCP broker.
 *
 * Commands arrive on a server-sent event stream rather than by polling, so an
 * idle tab costs one open connection instead of several requests per second.
 * The stream is authenticated by a path-scoped HttpOnly cookie, because
 * EventSource cannot send an Authorization header.
 */
export function useMcpBridge(
  apiBase: string,
  getSnapshot: () => unknown,
  onCommand: (command: McpCommand) => Promise<unknown> | unknown,
) {
  const snapshotRef = useRef(getSnapshot)
  const commandRef = useRef(onCommand)
  snapshotRef.current = getSnapshot
  commandRef.current = onCommand

  useEffect(() => {
    const controller = new AbortController()
    const viewId = crypto.randomUUID()
    let stream: EventSource | undefined
    let heartbeat = 0
    let retryTimer = 0
    let cleanupActivity = () => {}

    const run = async () => {
      // The session request installs the capability cookie that authenticates
      // both the stream and the write endpoints. A missing bridge is a silent
      // no-op: the app has to keep working with no backend.
      try {
        const response = await fetch(`${apiBase}/mcp/browser/session`, {
          cache: 'no-store',
          signal: controller.signal,
        })
        if (!response.ok) return
      } catch {
        return
      }
      if (controller.signal.aborted) return

      let viewSequence = 0
      const publishView = (timeoutMs?: number) => fetch(`${apiBase}/mcp/browser/view`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          viewId,
          sequence: ++viewSequence,
          active: document.visibilityState === 'visible' && document.hasFocus(),
          snapshot: snapshotRef.current(),
        }),
        signal: timeoutMs === undefined
          ? controller.signal
          : AbortSignal.any([controller.signal, AbortSignal.timeout(timeoutMs)]),
      })

      // The broker only streams to a view it already knows about.
      try {
        await publishView()
      } catch {
        return
      }
      if (controller.signal.aborted) return

      const publishActivity = () => { void publishView().catch(() => {}) }

      /**
       * Publishing before an acknowledgement is a contract, not a nicety: an
       * agent that reads straight after a tool call has to see the state that
       * call produced. A single dropped request would silently break it, so
       * this one is retried where the heartbeat is not.
       *
       * It is bounded, though. The caller is blocked waiting for its answer,
       * and a stalled publish must not turn a command that already succeeded
       * into a client-side timeout; the next heartbeat will carry the state.
       */
      const publishSnapshot = async () => {
        const deadline = Date.now() + PUBLISH_DEADLINE_MS
        for (let attempt = 0; attempt < MAX_PUBLISH_ATTEMPTS; attempt++) {
          const remaining = deadline - Date.now()
          if (controller.signal.aborted || remaining <= 0) return
          try {
            const response = await publishView(remaining)
            if (response.ok) return
          } catch {
            if (controller.signal.aborted) return
          }
          await delay(PUBLISH_RETRY_MS)
        }
      }
      document.addEventListener('visibilitychange', publishActivity)
      window.addEventListener('focus', publishActivity)
      window.addEventListener('blur', publishActivity)
      cleanupActivity = () => {
        document.removeEventListener('visibilitychange', publishActivity)
        window.removeEventListener('focus', publishActivity)
        window.removeEventListener('blur', publishActivity)
      }
      // The snapshot is the agent's view of the app, so it keeps its own
      // cadence independently of whether any command is in flight.
      heartbeat = window.setInterval(publishActivity, HEARTBEAT_INTERVAL_MS)

      let queue: Promise<void> = Promise.resolve()
      /** Commands already delivered, so a redelivery never runs twice. */
      const deliveries = new Map<string, { body?: string; done: boolean }>()

      const submitResult = async (id: string, body: string) => {
        for (let attempt = 0; attempt < MAX_RESULT_ATTEMPTS; attempt++) {
          if (controller.signal.aborted) return
          try {
            const response = await fetch(`${apiBase}/mcp/browser/result`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body,
              signal: controller.signal,
            })
            // 404 means the caller already gave up; nothing left to retry.
            if (response.ok || response.status === 404) {
              const delivery = deliveries.get(id)
              if (delivery) delivery.done = true
              return
            }
          } catch {
            if (controller.signal.aborted) return
          }
          await delay(RESULT_RETRY_MS)
        }
      }

      const handle = async (payload: string) => {
        let raw: unknown
        try {
          raw = JSON.parse(payload)
        } catch {
          return
        }
        const rawId = (raw as { id?: unknown }).id
        const deliveredId = typeof rawId === 'string' ? rawId : ''
        if (!deliveredId) return
        // An unacknowledged command is redelivered when the stream reconnects,
        // and may arrive while the original is still running. Applying the
        // edit twice would corrupt the track, so only the result is retried.
        const existing = deliveries.get(deliveredId)
        if (existing) {
          if (!existing.done && existing.body) await submitResult(deliveredId, existing.body)
          return
        }
        const delivery: { body?: string; done: boolean } = { done: false }
        deliveries.set(deliveredId, delivery)
        if (deliveries.size > MAX_TRACKED_COMMANDS) {
          deliveries.delete(deliveries.keys().next().value!)
        }

        let command: McpCommand | undefined
        let result: unknown
        let error = ''
        try {
          command = parseMcpCommand(raw)
          result = await commandRef.current(command)
        } catch (reason) {
          error = (reason as Error).message || 'MCP command failed'
        }
        if (!error) {
          await nextFrame()
          await publishSnapshot()
          if (controller.signal.aborted) return
        }
        delivery.body = JSON.stringify({
          viewId,
          id: command?.id ?? deliveredId,
          ...(error ? { error } : { result: result ?? { ok: true } }),
        })
        await submitResult(deliveredId, delivery.body)
      }

      const openStream = () => {
        if (controller.signal.aborted) return
        stream?.close()
        stream = new EventSource(
          `${apiBase}/mcp/browser/events?view_id=${encodeURIComponent(viewId)}`,
          { withCredentials: true },
        )
        // Commands are applied one at a time; overlapping edits would
        // interleave and report state that never existed.
        stream.onmessage = event => {
          queue = queue.then(() => handle(event.data)).catch(() => {})
        }
        stream.onerror = () => {
          // A dropped connection is retried by the browser itself. A closed
          // one means the broker refused the stream — usually because it
          // forgot this view — so re-announce it before trying again.
          if (controller.signal.aborted || stream?.readyState !== EventSource.CLOSED) return
          window.clearTimeout(retryTimer)
          retryTimer = window.setTimeout(() => {
            void publishView().catch(() => {}).finally(openStream)
          }, STREAM_RETRY_MS)
        }
      }
      openStream()
    }

    void run()
    return () => {
      controller.abort()
      window.clearInterval(heartbeat)
      window.clearTimeout(retryTimer)
      stream?.close()
      cleanupActivity()
    }
  }, [apiBase])
}

function delay(ms: number): Promise<void> {
  return new Promise(resolve => window.setTimeout(resolve, ms))
}

function nextFrame(): Promise<void> {
  return new Promise(resolve => requestAnimationFrame(() => resolve()))
}
