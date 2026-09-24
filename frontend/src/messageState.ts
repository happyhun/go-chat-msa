import { compareMessages, insertSorted, mergeSorted } from './messages.ts'
import type { MessageInfo } from './types.ts'

const SEND_CONFIRM_TIMEOUT_SECONDS = 10

type MessageAction =
  | { type: 'loaded'; messages: MessageInfo[] }
  | { type: 'received'; message: MessageInfo }
  | { type: 'synced'; messages: MessageInfo[] }
  | { type: 'timeout'; now: number }
  | { type: 'retried'; message: MessageInfo; now: number }

export function messageReducer(messages: MessageInfo[], action: MessageAction): MessageInfo[] {
  switch (action.type) {
    case 'loaded':
      return action.messages.toSorted(compareMessages)
    case 'received':
      return insertSorted(messages, action.message)
    case 'synced':
      return mergeSorted(messages, action.messages)
    case 'timeout': {
      let changed = false
      const next = messages.map((message) => {
        if (message.status !== 'sending' || action.now - message.timestamp < SEND_CONFIRM_TIMEOUT_SECONDS) return message
        changed = true
        return { ...message, status: 'failed' as const }
      })
      return changed ? next : messages
    }
    case 'retried':
      return messages.map((message) => message === action.message && message.status === 'failed'
        ? { ...message, status: 'sending', timestamp: action.now }
        : message)
  }
}
