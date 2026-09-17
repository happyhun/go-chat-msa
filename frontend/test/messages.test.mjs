import assert from 'node:assert/strict'
import test from 'node:test'
import {
  insertSorted,
  mergeSorted,
  rewindMessageId,
  lastMessageId,
  oldestRecoveryAnchor,
} from '../src/messages.ts'

function message(id, clientMessageId = id) {
  return {
    id,
    room_id: 'room',
    sender_id: 'sender',
    content: id,
    client_msg_id: clientMessageId,
    type: 'chat',
    timestamp: 0,
  }
}

test('insertSorted inserts out-of-order messages by id', () => {
  const messages = [message('1'), message('3')]
  assert.deepEqual(insertSorted(messages, message('2')).map(({ id }) => id), ['1', '2', '3'])
})

test('insertSorted replaces optimistic messages in id order without mutating the input', () => {
  const first = message('1')
  const last = message('3')
  const pending = { ...message('pending:client', 'client'), status: 'sending' }
  const messages = Object.freeze([first, last, pending])
  const received = message('2', 'client')

  assert.deepEqual(insertSorted(messages, received), [first, received, last])
  assert.deepEqual(messages, [first, last, pending])
})

test('insertSorted does not downgrade accepted messages on retry or timeout', () => {
  const accepted = { ...message('2', 'client'), status: 'accepted' }
  const messages = [accepted]

  for (const status of ['sending', 'failed']) {
    assert.strictEqual(insertSorted(messages, { ...message('pending:client', 'client'), status }), messages)
  }
})

test('mergeSorted sorts and removes id and client message duplicates', () => {
  const messages = [message('2', 'b')]
  const batch = [message('3', 'c'), message('1', 'a'), message('4', 'c'), message('2', 'd')]
  assert.deepEqual(mergeSorted(messages, batch).map(({ id }) => id), ['1', '2', '3'])
})

test('mergeSorted preserves the array when sync only repeats received messages', () => {
  const messages = [message('1'), message('2')]

  assert.strictEqual(mergeSorted(messages, []), messages)
  assert.strictEqual(mergeSorted(messages, [message('2'), message('1')]), messages)
})

test('mergeSorted replaces aliases once and preserves unrelated messages', () => {
  const receivedAlias = message('5', 'client-a')
  const acceptedAlias = { ...message('6', 'client-b'), status: 'accepted' }
  const unrelated = message('3', 'client-c')
  const messages = Object.freeze([unrelated, receivedAlias, acceptedAlias])
  const canonicalA = message('1', 'client-a')
  const canonicalB = message('2', 'client-b')
  const batch = Object.freeze([canonicalB, canonicalA, message('4', 'client-a'), message('2', 'client-b')])

  assert.deepEqual(mergeSorted(messages, batch), [canonicalA, canonicalB, unrelated])
  assert.deepEqual(messages, [unrelated, receivedAlias, acceptedAlias])
})

test('mergeSorted keeps system messages without client ids distinct', () => {
  const first = { ...message('1', ''), type: 'system' }
  const second = { ...message('2', ''), type: 'system' }

  assert.deepEqual(mergeSorted([first], [second, first]), [first, second])
})

test('rewindMessageId rewinds the UUIDv7 timestamp', () => {
  assert.equal(
    rewindMessageId('00000000-0bb8-7abc-8def-0123456789ab'),
    '00000000-03e8-7000-8000-000000000000',
  )
})

test('rewindMessageId clamps underflow and preserves invalid ids', () => {
  assert.equal(
    rewindMessageId('00000000-03e8-7000-8000-000000000000'),
    '00000000-0000-7000-8000-000000000000',
  )
  assert.equal(rewindMessageId('invalid'), 'invalid')
})

test('message deduplication is scoped to room and sender', () => {
  const first = message('1', 'client')
  const otherSender = { ...message('2', 'client'), sender_id: 'another' }
  const otherRoom = { ...message('3', 'client'), room_id: 'another' }

  assert.deepEqual(insertSorted([first], otherSender), [first, otherSender])
  assert.deepEqual(insertSorted([first], otherRoom), [first, otherRoom])
  assert.deepEqual(mergeSorted([first], [otherSender, otherRoom]), [first, otherSender, otherRoom])
})

test('canonical DB message replaces accepted alias after idempotency record loss', () => {
  const accepted = { ...message('2', 'client'), status: 'accepted' }
  const canonical = message('1', 'client')
  assert.deepEqual(mergeSorted([accepted], [canonical]), [canonical])
})

test('optimistic messages never advance recovery cursor', () => {
  assert.equal(lastMessageId([message('1'), { ...message('pending:client'), status: 'sending' }]), '1')
})

test('late acceptance or failure cannot overwrite a received message', () => {
  const received = message('1', 'client')
  assert.equal(insertSorted([received], { ...message('2', 'client'), status: 'accepted' })[0], received)
  assert.equal(insertSorted([received], { ...message('pending:client', 'client'), status: 'failed' })[0], received)
})

test('recovery anchor retains the earliest unresolved range', () => {
  assert.equal(oldestRecoveryAnchor(null, '2'), '2')
  assert.equal(oldestRecoveryAnchor('1', '3'), '1')
  assert.equal(oldestRecoveryAnchor('3', '1'), '1')
  assert.equal(oldestRecoveryAnchor('', '3'), '')
})
