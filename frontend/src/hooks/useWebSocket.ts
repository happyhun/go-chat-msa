import { useRef, useCallback, useEffect, useState } from 'react'
import type { WsOutgoing } from '../types'
import { ApiError, createWsTicket } from '../api/client'

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

export type SendResult =
  | { ok: true; queued: boolean; clientMsgId: string }
  | { ok: false; reason: 'queue_full'; clientMsgId: string }

export type WebSocketStopReason =
  | 'auth'
  | 'rate_limited'
  | 'service_unavailable'
  | 'connection_failed'
  | 'ticket_failed'

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
  onGaveUp?: (reason: WebSocketStopReason) => void
}

function ticketFailureReason(err: unknown): WebSocketStopReason {
  if (!(err instanceof ApiError)) return 'ticket_failed'
  if (err.status === 401) return 'auth'
  if (err.status === 429) return 'rate_limited'
  if ([502, 503, 504].includes(err.status)) return 'service_unavailable'
  return 'ticket_failed'
}

export function useWebSocket({ roomId, onMessage, onConflict, onReconnected, onGaveUp }: UseWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const [reconnecting, setReconnecting] = useState(false)
  const [queuedCount, setQueuedCount] = useState(0)
  const attemptRef = useRef(0)
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined)
  const mountedRef = useRef(true)
  const shouldReconnectRef = useRef(true)
  const acceptingDirectSendRef = useRef(false)
  const sendQueueRef = useRef<OutboundMessage[]>([])
  const pendingRef = useRef<Map<string, OutboundMessage>>(new Map())
  const connectOnceRef = useRef<() => Promise<boolean>>(async () => false)

  const onMessageRef = useRef(onMessage)
  const onConflictRef = useRef(onConflict)
  const onReconnectedRef = useRef(onReconnected)
  const onGaveUpRef = useRef(onGaveUp)

  useEffect(() => {
    onMessageRef.current = onMessage
    onConflictRef.current = onConflict
    onReconnectedRef.current = onReconnected
    onGaveUpRef.current = onGaveUp
  }, [onMessage, onConflict, onReconnected, onGaveUp])

  const updateQueuedCount = useCallback(() => {
    setQueuedCount(sendQueueRef.current.length)
  }, [])

  const queueMessages = useCallback((messages: OutboundMessage[]) => {
    const existing = new Set(sendQueueRef.current.map((msg) => msg.client_msg_id))
    let queued = 0
    for (const msg of messages) {
      if (existing.has(msg.client_msg_id)) continue
      if (sendQueueRef.current.length >= MAX_SEND_QUEUE_SIZE) break
      sendQueueRef.current.push(msg)
      existing.add(msg.client_msg_id)
      queued += 1
    }
    updateQueuedCount()
    return queued
  }, [updateQueuedCount])

  const movePendingToQueue = useCallback(() => {
    const pending = Array.from(pendingRef.current.values())
    if (pending.length === 0) return
    pendingRef.current.clear()
    const queued = sendQueueRef.current
    sendQueueRef.current = []
    queueMessages([...pending, ...queued])
  }, [queueMessages])

  const scheduleReconnect = useCallback(() => {
    if (!mountedRef.current || !shouldReconnectRef.current) return
    if (attemptRef.current >= MAX_RECONNECT_ATTEMPTS) {
      shouldReconnectRef.current = false
      acceptingDirectSendRef.current = false
      sendQueueRef.current = []
      updateQueuedCount()
      setReconnecting(false)
      onGaveUpRef.current?.('connection_failed')
      return
    }

    setReconnecting(true)
    attemptRef.current += 1
    const cap = Math.min(INITIAL_RECONNECT_MAX_MS * 2 ** (attemptRef.current - 1), MAX_DELAY_MS)
    const delay = Math.max(INITIAL_RECONNECT_MIN_MS, Math.random() * cap)

    timerRef.current = setTimeout(() => {
      if (mountedRef.current && shouldReconnectRef.current) {
        connectOnceRef.current()
      }
    }, delay)
  }, [updateQueuedCount])

  const connectOnce = useCallback(async (): Promise<boolean> => {
    if (!mountedRef.current) return false

    let ticket: string
    try {
      const res = await createWsTicket()
      ticket = res.ticket
    } catch (err) {
      // 티켓 발급 실패는 HTTP 상태를 보고 사용자에게 다른 복구 경로를 안내한다.
      if (mountedRef.current) {
        shouldReconnectRef.current = false
        acceptingDirectSendRef.current = false
        sendQueueRef.current = []
        updateQueuedCount()
        setReconnecting(false)
        onGaveUpRef.current?.(ticketFailureReason(err))
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
        updateQueuedCount()
        if (wasReconnect) {
          try {
            const caughtUp = await onReconnectedRef.current?.()
            const caughtUpClientMsgIds = normalizeClientMsgIds(caughtUp)
            if (caughtUpClientMsgIds.size > 0) {
              queued = queued.filter((msg) => !caughtUpClientMsgIds.has(msg.client_msg_id))
            }
          } catch {
            sendQueueRef.current = [...queued, ...sendQueueRef.current].slice(0, MAX_SEND_QUEUE_SIZE)
            updateQueuedCount()
            acceptingDirectSendRef.current = false
            ws.close()
            resolve(false)
            return
          }
        }
        if (!mountedRef.current || ws.readyState !== WebSocket.OPEN) {
          sendQueueRef.current = [...queued, ...sendQueueRef.current].slice(0, MAX_SEND_QUEUE_SIZE)
          updateQueuedCount()
          resolve(false)
          return
        }
        queued = [...queued, ...sendQueueRef.current].slice(0, MAX_SEND_QUEUE_SIZE)
        sendQueueRef.current = []
        updateQueuedCount()
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
        let msg: WsOutgoing
        try {
          msg = JSON.parse(ev.data)
        } catch {
          ws.close()
          return
        }
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
  }, [movePendingToQueue, roomId, scheduleReconnect, updateQueuedCount])

  useEffect(() => {
    connectOnceRef.current = connectOnce
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

  const send = useCallback((content: string): SendResult => {
    const msg: OutboundMessage = {
      content,
      client_msg_id: crypto.randomUUID(),
      type: 'chat',
    }
    if (acceptingDirectSendRef.current && wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      pendingRef.current.set(msg.client_msg_id, msg)
      wsRef.current.send(JSON.stringify(msg))
      return { ok: true, queued: false, clientMsgId: msg.client_msg_id }
    }
    const queued = queueMessages([msg])
    if (queued === 0) {
      return { ok: false, reason: 'queue_full', clientMsgId: msg.client_msg_id }
    }
    return { ok: true, queued: true, clientMsgId: msg.client_msg_id }
  }, [queueMessages])

  const disconnect = useCallback(() => {
    shouldReconnectRef.current = false
    acceptingDirectSendRef.current = false
    setReconnecting(false)
    clearTimeout(timerRef.current)
    pendingRef.current.clear()
    sendQueueRef.current = []
    updateQueuedCount()
    wsRef.current?.close()
    wsRef.current = null
  }, [updateQueuedCount])

  useEffect(() => {
    const pending = pendingRef.current
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      shouldReconnectRef.current = false
      acceptingDirectSendRef.current = false
      clearTimeout(timerRef.current)
      pending.clear()
      sendQueueRef.current = []
      setQueuedCount(0)
      wsRef.current?.close()
    }
  }, [])

  return { connected, reconnecting, queuedCount, connect, disconnect, send }
}
