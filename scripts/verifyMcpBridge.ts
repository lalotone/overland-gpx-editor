import assert from 'node:assert/strict'
import { test } from 'node:test'
import { JSDOM } from 'jsdom'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { useMcpBridge } from '../src/useMcpBridge'

const cases = [
  { name: 'HTML fallback', session: () => new Response('<html>app</html>'), puts: 0, connected: false },
  { name: 'disabled session', session: () => Response.json({ enabled: false }), puts: 0, connected: false },
  { name: 'missing backend', session: () => new Response(null, { status: 404 }), puts: 0, connected: false },
  { name: 'network failure', session: () => { throw new TypeError('offline') }, puts: 0, connected: false },
  { name: 'failed initial publish', session: () => Response.json({ enabled: true }), status: 405, puts: 1, connected: false },
  { name: 'enabled bridge', session: () => Response.json({ enabled: true }), puts: 1, connected: true },
]

for (const scenario of cases) {
  test(`MCP startup: ${scenario.name}`, async t => {
    const dom = new JSDOM('<div id="root"></div>')
    const originals = new Map<string, PropertyDescriptor | undefined>()
    const install = (name: string, value: unknown) => {
      originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name))
      Object.defineProperty(globalThis, name, { configurable: true, writable: true, value })
    }
    install('window', dom.window)
    install('document', dom.window.document)
    install('IS_REACT_ACT_ENVIRONMENT', true)
    let puts = 0
    let streams = 0
    let closed = 0
    install('fetch', async (_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        puts++
        return new Response(null, { status: scenario.status ?? 204 })
      }
      return scenario.session()
    })
    install('EventSource', class {
      constructor() { streams++ }
      close() { closed++ }
    })
    const intervals = new Map<number, () => void>()
    dom.window.setInterval = (callback: () => void) => {
      intervals.set(1, callback)
      return 1
    }
    dom.window.clearInterval = (id: number) => { intervals.delete(id) }
    const root = createRoot(dom.window.document.getElementById('root')!)
    t.after(async () => {
      await act(async () => root.unmount())
      dom.window.close()
      for (const [name, descriptor] of originals) {
        if (descriptor) Object.defineProperty(globalThis, name, descriptor)
        else Reflect.deleteProperty(globalThis, name)
      }
    })
    function Page() {
      useMcpBridge('', () => ({ mode: 'planner' }), () => {})
      return null
    }
    await act(async () => { root.render(createElement(Page)) })
    assert.equal(puts, scenario.puts, 'initial publishes')
    assert.equal(streams, scenario.connected ? 1 : 0, 'command streams')
    assert.equal(intervals.size, scenario.connected ? 1 : 0, 'heartbeat timers')
    await act(async () => {
      for (const heartbeat of intervals.values()) heartbeat()
      dom.window.dispatchEvent(new dom.window.Event('focus'))
    })
    assert.equal(puts, scenario.puts + (scenario.connected ? 2 : 0), 'heartbeat and activity publishes')
    await act(async () => root.unmount())
    assert.equal(intervals.size, 0, 'heartbeat cleaned up')
    assert.equal(closed, streams, 'streams cleaned up')
  })
}
