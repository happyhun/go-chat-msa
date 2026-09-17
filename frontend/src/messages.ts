import type { MessageInfo } from './types'

const UUID_V7_PATTERN = /^([0-9a-f]{8})-([0-9a-f]{4})-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i

export const SYNC_REWIND_MS = 2000

export function oldestRecoveryAnchor(existing: string | null, incoming: string): string {
  return existing === null ? incoming : existing < incoming ? existing : incoming
}

export function compareMessages(a: MessageInfo, b: MessageInfo): number {
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

export function isSameMessage(a: MessageInfo, b: MessageInfo): boolean {
  return a.id === b.id || Boolean(
    a.client_msg_id && b.client_msg_id && a.client_msg_id === b.client_msg_id &&
    a.room_id === b.room_id && a.sender_id === b.sender_id,
  )
}

export function insertSorted(messages: MessageInfo[], message: MessageInfo): MessageInfo[] {
  const existing = messages.find((current) => isSameMessage(current, message))
  if (existing) {
    if (!existing.status) return messages
    if (message.status && existing.status === 'accepted' && message.status !== 'accepted') return messages
    messages = messages.filter((current) => current !== existing)
  }
  if (messages.length === 0 || compareMessages(messages[messages.length - 1], message) <= 0) {
    return [...messages, message]
  }

  let low = 0
  let high = messages.length
  while (low < high) {
    const middle = (low + high) >>> 1
    if (compareMessages(messages[middle], message) < 0) low = middle + 1
    else high = middle
  }

  return [...messages.slice(0, low), message, ...messages.slice(low)]
}

export function mergeSorted(messages: MessageInfo[], batch: MessageInfo[]): MessageInfo[] {
  if (batch.length === 0) return messages

  const byId = new Map(messages.map((message) => [message.id, message]))
  const byClientMessage = new Map(messages.map((message) => [clientMessageKey(message), message]))
  const canonicalMessages = new Set<MessageInfo>()

  for (const message of batch) {
    const key = clientMessageKey(message)
    const existing = byId.get(message.id) ?? (key ? byClientMessage.get(key) : undefined)
    if (existing) {
      if (canonicalMessages.has(existing)) continue
      if (!existing.status && existing.id === message.id) continue
      byId.delete(existing.id)
      byClientMessage.delete(clientMessageKey(existing))
    }
    byId.set(message.id, message)
    if (key) byClientMessage.set(key, message)
    canonicalMessages.add(message)
  }

  return canonicalMessages.size === 0 ? messages : [...byId.values()].sort(compareMessages)
}

export function clientMessageKey(message: MessageInfo): string {
  return message.client_msg_id ? `${message.room_id}:${message.sender_id}:${message.client_msg_id}` : ''
}

export function lastMessageId(messages: MessageInfo[]): string {
  let last = ''
  for (const message of messages) {
    if (message.id.startsWith('pending:')) continue
    if (message.id > last) last = message.id
  }
  return last
}

export function rewindMessageId(id: string, rewindMs = SYNC_REWIND_MS): string {
  const match = UUID_V7_PATTERN.exec(id)
  if (!match) return id

  const timestamp = Number.parseInt(match[1] + match[2], 16)
  const rewound = Math.max(0, timestamp - rewindMs).toString(16).padStart(12, '0')
  return `${rewound.slice(0, 8)}-${rewound.slice(8)}-7000-8000-000000000000`
}
