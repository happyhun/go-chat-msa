import assert from 'node:assert/strict'
import test from 'node:test'
import { messageReducer } from '../src/messageState.ts'

const pending = Object.freeze({
  id: 'pending:client', room_id: 'room', sender_id: 'sender',
  content: 'hello', client_msg_id: 'client', type: 'chat',
  timestamp: 100, status: 'sending',
})

test('timeout preserves recent sends and fails only unconfirmed messages at the deadline', () => {
  const accepted = { ...pending, id: '2', status: 'accepted' }
  const received = { ...pending, id: '3', status: undefined }
  const messages = Object.freeze([pending, accepted, received])
  assert.equal(messageReducer(messages, { type: 'timeout', now: 109.9 }), messages)
  const next = messageReducer(messages, { type: 'timeout', now: 110 })
  assert.equal(next[0].status, 'failed')
  assert.equal(next[1], accepted)
  assert.equal(next[2], received)
  assert.equal(pending.status, 'sending')
})

test('retry preserves the client id and restarts the confirmation deadline', () => {
  const failed = { ...pending, status: 'failed' }
  const retried = messageReducer([failed], { type: 'retried', message: failed, now: 200 })
  assert.equal(retried[0].client_msg_id, 'client')
  assert.equal(retried[0].status, 'sending')
  assert.equal(messageReducer(retried, { type: 'timeout', now: 209 }), retried)
  assert.equal(messageReducer(retried, { type: 'timeout', now: 210 })[0].status, 'failed')
})

test('late delivery wins over a queued retry and timeout', () => {
  const failed = { ...pending, status: 'failed' }
  const received = { ...pending, id: '1', status: undefined }
  let messages = messageReducer([failed], { type: 'received', message: received })
  messages = messageReducer(messages, { type: 'retried', message: failed, now: 200 })
  messages = messageReducer(messages, { type: 'timeout', now: 300 })
  assert.deepEqual(messages, [received])
})

test('recovery replaces an optimistic message without duplication', () => {
  const received = { ...pending, id: '1', status: undefined }
  const messages = messageReducer([pending], { type: 'synced', messages: [received] })
  assert.deepEqual(messages, [received])
  assert.equal(messageReducer(messages, { type: 'received', message: received }), messages)
})

test('initial loading sorts without mutating the API response', () => {
  const messages = Object.freeze([{ ...pending, id: '2' }, { ...pending, id: '1' }])
  const sorted = messageReducer([], { type: 'loaded', messages })
  assert.deepEqual(sorted.map(({ id }) => id), ['1', '2'])
  assert.deepEqual(messages.map(({ id }) => id), ['2', '1'])
})
