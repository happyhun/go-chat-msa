import { useRef, useCallback, useEffect, useState } from 'react'
import type { WsOutgoing } from '../types'
import { createWsTicket } from '../api/client'

const MAX_RECONNECT_ATTEMPTS = 20
const INITIAL_RECONNECT_MIN_MS = 500
const INITIAL_RECONNECT_MAX_MS = 1000
const MAX_DELAY_MS = 30000
const MAX_SEND_QUEUE_SIZE = 50

type OutboundMessage = {
  content: string
  client_msg_id: string
  type: string
}

function normalizeClientMsgIds(value: void | string[] | Set<string>): Set<string> {
  if (!value) return new Set()
  if (value instanceof Set) return value
  return new Set(value)
}

interface UseWebSocketOptions {
  roomId: string
  onMessage: (msg: WsOutgoing) => void
  onConflict?: () => void
  onReconnected?: () => void | string[] | Set<string> | Promise<void | string[] | Set<string>>
  onGaveUp?: () => void
}

export function useWebSocket({ roomId, onMessage, onConflict, onReconnected, onGaveUp }: UseWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const [reconnecting, setReconnecting] = useState(false)
  const attemptRef = useRef(0)
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined)
  const mountedRef = useRef(true)
  const shouldReconnectRef = useRef(true)
  const acceptingDirectSendRef = useRef(false)
  const sendQueueRef = useRef<OutboundMessage[]>([])
  const pendingRef = useRef<Map<string, OutboundMessage>>(new Map())

  const onMessageRef = useRef(onMessage)
  const onConflictRef = useRef(onConflict)
  const onReconnectedRef = useRef(onReconnected)
  const onGaveUpRef = useRef(onGaveUp)
  onMessageRef.current = onMessage
  onConflictRef.current = onConflict
  onReconnectedRef.current = onReconnected
  onGaveUpRef.current = onGaveUp

  const queueMessages = useCallback((messages: OutboundMessage[]) => {
    const existing = new Set(sendQueueRef.current.map((msg) => msg.client_msg_id))
    for (const msg of messages) {
      if (existing.has(msg.client_msg_id)) continue
      if (sendQueueRef.current.length >= MAX_SEND_QUEUE_SIZE) break
      sendQueueRef.current.push(msg)
      existing.add(msg.client_msg_id)
    }
  }, [])

  const movePendingToQueue = useCallback(() => {
    const pending = Array.from(pendingRef.current.values())
    if (pending.length === 0) return
    pendingRef.current.clear()
    const queued = sendQueueRef.current
    sendQueueRef.current = []
    queueMessages([...pending, ...queued])
  }, [queueMessages])

  const connectOnce = useCallback(async (): Promise<boolean> => {
    if (!mountedRef.current) return false

    let ticket: string
    try {
      const res = await createWsTicket()
      ticket = res.ticket
    } catch {
      // 티켓 발급 실패 (토큰 만료, 방 삭제 등) — 재접속 중단
      if (mountedRef.current) {
        shouldReconnectRef.current = false
        onGaveUpRef.current?.()
      }
      return false
    }

    if (!mountedRef.current) return false

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${proto}//${window.location.host}/ws?room_id=${roomId}&ticket=${ticket}`

    return new Promise<boolean>((resolve) => {
      const ws = new WebSocket(url)
      wsRef.current = ws

      ws.onopen = async () => {
        if (!mountedRef.current) {
          ws.close()
          resolve(false)
          return
        }
        const wasReconnect = attemptRef.current > 0
        acceptingDirectSendRef.current = !wasReconnect
        let queued = sendQueueRef.current
        sendQueueRef.current = []
        if (wasReconnect) {
          try {
            const caughtUp = await onReconnectedRef.current?.()
            const caughtUpClientMsgIds = normalizeClientMsgIds(caughtUp)
            if (caughtUpClientMsgIds.size > 0) {
              queued = queued.filter((msg) => !caughtUpClientMsgIds.has(msg.client_msg_id))
            }
          } catch {
            sendQueueRef.current = [...queued, ...sendQueueRef.current].slice(0, MAX_SEND_QUEUE_SIZE)
            acceptingDirectSendRef.current = false
            ws.close()
            resolve(false)
            return
          }
        }
        if (!mountedRef.current || ws.readyState !== WebSocket.OPEN) {
          sendQueueRef.current = [...queued, ...sendQueueRef.current].slice(0, MAX_SEND_QUEUE_SIZE)
          resolve(false)
          return
        }
        queued = [...queued, ...sendQueueRef.current].slice(0, MAX_SEND_QUEUE_SIZE)
        sendQueueRef.current = []
        for (const msg of queued) {
          pendingRef.current.set(msg.client_msg_id, msg)
          ws.send(JSON.stringify(msg))
        }
        setConnected(true)
        attemptRef.current = 0
        acceptingDirectSendRef.current = true
        if (wasReconnect) {
          setReconnecting(false)
        }
        resolve(true)
      }

      ws.onmessage = (ev) => {
        const msg: WsOutgoing = JSON.parse(ev.data)
        if (msg.type === 'conflict') {
          shouldReconnectRef.current = false
          acceptingDirectSendRef.current = false
          onConflictRef.current?.()
          return
        }
        if (msg.client_msg_id) {
          pendingRef.current.delete(msg.client_msg_id)
        }
        onMessageRef.current(msg)
      }

      ws.onerror = () => {
        resolve(false)
      }

      ws.onclose = () => {
        setConnected(false)
        acceptingDirectSendRef.current = false
        wsRef.current = null
        movePendingToQueue()
        if (mountedRef.current && shouldReconnectRef.current) {
          scheduleReconnect()
        }
      }
    })
  }, [movePendingToQueue, queueMessages, roomId])

  const scheduleReconnect = useCallback(() => {
    if (!mountedRef.current || !shouldReconnectRef.current) return
    if (attemptRef.current >= MAX_RECONNECT_ATTEMPTS) {
      setReconnecting(false)
      onGaveUpRef.current?.()
      return
    }

    setReconnecting(true)
    attemptRef.current += 1
    const cap = Math.min(INITIAL_RECONNECT_MAX_MS * 2 ** (attemptRef.current - 1), MAX_DELAY_MS)
    const delay = Math.max(INITIAL_RECONNECT_MIN_MS, Math.random() * cap)

    timerRef.current = setTimeout(() => {
      if (mountedRef.current && shouldReconnectRef.current) {
        connectOnce()
      }
    }, delay)
  }, [connectOnce])

  const connect = useCallback(async () => {
    shouldReconnectRef.current = true
    attemptRef.current = 0
    acceptingDirectSendRef.current = false
    setReconnecting(false)
    if (wsRef.current) {
      wsRef.current.close()
    }
    await connectOnce()
  }, [connectOnce])

  const send = useCallback((content: string) => {
    const msg: OutboundMessage = {
      content,
      client_msg_id: crypto.randomUUID(),
      type: 'chat',
    }
    if (acceptingDirectSendRef.current && wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      pendingRef.current.set(msg.client_msg_id, msg)
      wsRef.current.send(JSON.stringify(msg))
      return
    }
    queueMessages([msg])
  }, [queueMessages])

  const disconnect = useCallback(() => {
    shouldReconnectRef.current = false
    acceptingDirectSendRef.current = false
    setReconnecting(false)
    clearTimeout(timerRef.current)
    pendingRef.current.clear()
    sendQueueRef.current = []
    wsRef.current?.close()
    wsRef.current = null
  }, [])

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      shouldReconnectRef.current = false
      acceptingDirectSendRef.current = false
      clearTimeout(timerRef.current)
      pendingRef.current.clear()
      sendQueueRef.current = []
      wsRef.current?.close()
    }
  }, [])

  return { connected, reconnecting, queuedCount: sendQueueRef.current.length, connect, disconnect, send }
}
