// Minimal Chrome DevTools client for the app's Android WebView, for device
// checks where Playwright's connectOverCDP cannot attach (see
// device-smoke-cdp.mjs). Attaches to the Overland page target directly.
import { writeFileSync } from 'node:fs'

export async function attach(port = 9222) {
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json()
  const page = targets.find(t => t.type === 'page' && t.url.startsWith('http://127.0.0.1:'))
  if (!page) throw new Error('Overland WebView page not found: ' + JSON.stringify(targets.map(t => t.url)))
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject })
  let id = 0
  const pending = new Map()
  const errors = []
  ws.onmessage = event => {
    const msg = JSON.parse(event.data)
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id)
      pending.delete(msg.id)
      msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result)
    } else if (msg.method === 'Runtime.exceptionThrown') {
      errors.push(msg.params.exceptionDetails?.exception?.description ?? msg.params.exceptionDetails?.text)
    }
  }
  const send = (method, params = {}) => new Promise((resolve, reject) => {
    const n = ++id
    pending.set(n, { resolve, reject })
    ws.send(JSON.stringify({ id: n, method, params }))
  })
  await send('Runtime.enable')
  await send('Page.enable')
  const evaluate = async (expression, timeout = 600000) => {
    const result = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, timeout })
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text)
    return result.result.value
  }
  return {
    url: page.url,
    errors,
    send,
    evaluate,
    async screenshot(file) {
      const { data } = await send('Page.captureScreenshot', { format: 'png' })
      writeFileSync(file, Buffer.from(data, 'base64'))
    },
    /** Clicks the first visible element matching an accessible label or text. */
    async click(label) {
      const ok = await evaluate(`(() => {
        const want = ${JSON.stringify(label)}
        const all = [...document.querySelectorAll('button, [role=button], a, [role=tab], label')]
        const el = all.find(e => (e.getAttribute('aria-label') === want || e.textContent.trim() === want) && e.offsetParent !== null)
          ?? all.find(e => (e.getAttribute('aria-label') ?? e.textContent).trim().startsWith(want) && e.offsetParent !== null)
        if (!el) return false
        el.click()
        return true
      })()`)
      if (!ok) throw new Error('No clickable element: ' + label)
    },
    wait: ms => new Promise(resolve => setTimeout(resolve, ms)),
    close: () => ws.close(),
  }
}
