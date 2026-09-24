import { delay } from './retry.ts'

interface Options<T> {
  fetch: (signal: AbortSignal) => Promise<T>
  onSuccess: (value: T) => void
  onError: (error: unknown) => void
  retryDelay: (error: unknown, attempt: number) => number | null
}

export function createRefreshQueue<T>({ fetch, onSuccess, onError, retryDelay }: Options<T>) {
  const controller = new AbortController()
  let running = false
  let pending = false

  async function run() {
    running = true
    let failures = 0
    try {
      while (pending && !controller.signal.aborted) {
        pending = false
        try {
          const value = await fetch(controller.signal)
          if (controller.signal.aborted) return
          // A newer event invalidates the snapshot requested before that event.
          if (!pending) onSuccess(value)
          failures = 0
        } catch (error) {
          if (controller.signal.aborted) return
          onError(error)
          const wait = retryDelay(error, failures++)
          if (wait === null || failures >= 3) return
          await delay(wait, controller.signal)
          pending = true
        }
      }
    } catch (error) {
      if (!controller.signal.aborted) onError(error)
    } finally {
      running = false
    }
  }

  return {
    refresh() {
      if (controller.signal.aborted) return
      pending = true
      if (!running) void run()
    },
    dispose() {
      controller.abort()
    },
  }
}
