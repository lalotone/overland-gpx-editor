export type Fetcher = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>

function abortError(): Error {
  const error = new Error('Aborted')
  error.name = 'AbortError'
  return error
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(abortError())

  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    const onAbort = () => {
      clearTimeout(timer)
      reject(abortError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

/** Serialize requests and leave at least `intervalMs` between their starts. */
export function createRateLimitedFetch(intervalMs: number, fetcher: Fetcher = fetch): Fetcher {
  let queue: Promise<void> = Promise.resolve()
  let nextStart = 0

  return (input, init) => {
    const run = async () => {
      const signal = init?.signal ?? undefined
      if (signal?.aborted) throw abortError()

      const delay = nextStart - Date.now()
      if (delay > 0) await wait(delay, signal)
      if (signal?.aborted) throw abortError()

      nextStart = Date.now() + intervalMs
      return fetcher(input, init)
    }

    const result = queue.then(run, run)
    queue = result.then(() => undefined, () => undefined)
    return result
  }
}
