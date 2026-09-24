import assert from 'node:assert/strict'
import test from 'node:test'
import { setImmediate } from 'node:timers/promises'
import { createRefreshQueue } from '../src/refreshQueue.ts'

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

test('overlapping invalidations serialize requests and discard the stale snapshot', async () => {
  const requests = [], applied = []
  const queue = createRefreshQueue({
    fetch: () => { const request = deferred(); requests.push(request); return request.promise },
    onSuccess: value => applied.push(value), onError: assert.fail, retryDelay: () => null,
  })
  queue.refresh()
  queue.refresh()
  queue.refresh()
  assert.equal(requests.length, 1)
  requests[0].resolve(['old'])
  await setImmediate()
  assert.deepEqual(applied, [])
  assert.equal(requests.length, 2)
  requests[1].resolve(['latest'])
  await setImmediate()
  assert.deepEqual(applied, [['latest']])
  queue.dispose()
})

test('new refresh events cannot bypass the retry delay', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  let calls = 0
  const applied = []
  const queue = createRefreshQueue({
    fetch: async () => { if (++calls === 1) throw new Error('429'); return ['latest'] },
    onSuccess: value => applied.push(value), onError: () => {}, retryDelay: () => 2000,
  })
  queue.refresh()
  await setImmediate()
  queue.refresh()
  t.mock.timers.tick(1999)
  await setImmediate()
  assert.equal(calls, 1)
  t.mock.timers.tick(1)
  await setImmediate()
  assert.equal(calls, 2)
  assert.deepEqual(applied, [['latest']])
  queue.dispose()
})

test('transient errors stop after three attempts and manual refresh can recover', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  let calls = 0, failing = true
  const applied = []
  const queue = createRefreshQueue({
    fetch: async () => { calls++; if (failing) throw new Error('503'); return ['recovered'] },
    onSuccess: value => applied.push(value), onError: () => {}, retryDelay: () => 1000,
  })
  queue.refresh()
  await setImmediate()
  for (let i = 0; i < 5; i++) { t.mock.timers.tick(1000); await setImmediate() }
  assert.equal(calls, 3)
  failing = false
  queue.refresh()
  await setImmediate()
  assert.deepEqual(applied, [['recovered']])
  queue.dispose()
})

test('disposal aborts pending requests and suppresses their late results', async () => {
  const request = deferred()
  let signal, calls = 0
  const queue = createRefreshQueue({
    fetch: active => { signal = active; calls++; return request.promise },
    onSuccess: assert.fail, onError: assert.fail, retryDelay: () => 1000,
  })
  queue.refresh()
  queue.dispose()
  assert.equal(signal.aborted, true)
  request.resolve(['late'])
  await setImmediate()
  queue.refresh()
  assert.equal(calls, 1)
})

test('disposal cancels a scheduled retry', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  let calls = 0
  const queue = createRefreshQueue({
    fetch: async () => { calls++; throw new Error('offline') },
    onSuccess: assert.fail, onError: () => {}, retryDelay: () => 1000,
  })
  queue.refresh()
  await setImmediate()
  queue.dispose()
  t.mock.timers.tick(10000)
  await setImmediate()
  assert.equal(calls, 1)
})

test('permanent errors do not automatically retry', async () => {
  let calls = 0, errors = 0
  const queue = createRefreshQueue({
    fetch: async () => { calls++; throw new Error('403') },
    onSuccess: assert.fail, onError: () => errors++, retryDelay: () => null,
  })
  queue.refresh()
  await setImmediate()
  assert.equal(calls, 1)
  assert.equal(errors, 1)
  queue.dispose()
})
