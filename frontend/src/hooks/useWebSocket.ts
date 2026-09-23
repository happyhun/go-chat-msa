import { useRef, useCallback, useEffect, useState } from 'react'
import type { WsOutgoing } from '../types'
import { ApiError, createWsTicket } from '../api/client'

const MAX_RECONNECT_ATTEMPTS = 20
const STABLE_CONNECTION_MS = 30000

export type WebSocketStopReason = 'auth' | 'rate_limited' | 'connection_failed'

interface UseWebSocketOptions {
  roomId: string
  onMessage: (msg: WsOutgoing) => void
  onConnected?: () => void
  onGaveUp?: (reason: WebSocketStopReason) => void
}

export function useWebSocket({ roomId, onMessage, onConnected, onGaveUp }: UseWebSocketOptions) {
  const [connected, setConnected] = useState(false)
  const [reconnecting, setReconnecting] = useState(false)
  const wsRef = useRef<WebSocket | null>(null)
  const attempts = useRef(0)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const enabled = useRef(false)
  const generation = useRef(0)
  const callbacks = useRef({ onMessage, onConnected, onGaveUp })
  const connectRef = useRef<() => Promise<void>>(async () => {})

  useEffect(() => {
    callbacks.current = { onMessage, onConnected, onGaveUp }
  }, [onMessage, onConnected, onGaveUp])

  const scheduleReconnect = useCallback(() => {
    if (!enabled.current) return
    if (attempts.current >= MAX_RECONNECT_ATTEMPTS) {
      enabled.current = false
      setReconnecting(false)
      callbacks.current.onGaveUp?.('connection_failed')
      return
    }
    setReconnecting(true)
    const cap = Math.min(1000 * 2 ** attempts.current++, 30000)
    timer.current = setTimeout(() => { void connectRef.current() }, Math.max(500, Math.random() * cap))
  }, [])

  const connectOnce = useCallback(async () => {
    const connectionGeneration = generation.current
    let ticket: string
    try {
      ticket = (await createWsTicket()).ticket
    } catch (err) {
      if (!enabled.current || connectionGeneration !== generation.current) return
      if (err instanceof ApiError && [401, 403, 429].includes(err.status)) {
        enabled.current = false
        setReconnecting(false)
        callbacks.current.onGaveUp?.(err.status === 429 ? 'rate_limited' : 'auth')
      } else {
        scheduleReconnect()
      }
      return
    }
    if (!enabled.current || connectionGeneration !== generation.current) return
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${protocol}//${window.location.host}/ws?room_id=${roomId}&ticket=${ticket}`)
    wsRef.current = ws
    let openedAt: number | undefined
    await new Promise<void>((resolve) => {
      ws.onopen = () => {
        if (!enabled.current || connectionGeneration !== generation.current) {
          ws.close()
          resolve()
          return
        }
        openedAt = performance.now()
        setConnected(true)
        setReconnecting(false)
        callbacks.current.onConnected?.()
        resolve()
      }
      ws.onmessage = (event) => {
        if (connectionGeneration !== generation.current) return
        let msg: WsOutgoing
        try {
          msg = JSON.parse(event.data)
        } catch {
          ws.close()
          return
        }
        callbacks.current.onMessage(msg)
      }
      ws.onerror = () => resolve()
      ws.onclose = () => {
        resolve()
        if (connectionGeneration !== generation.current) return
        if (openedAt !== undefined && performance.now() - openedAt >= STABLE_CONNECTION_MS) {
          attempts.current = 0
        }
        setConnected(false)
        wsRef.current = null
        scheduleReconnect()
      }
    })
  }, [roomId, scheduleReconnect])

  useEffect(() => { connectRef.current = connectOnce }, [connectOnce])

  const disconnect = useCallback(() => {
    enabled.current = false
    generation.current++
    clearTimeout(timer.current)
    wsRef.current?.close()
    wsRef.current = null
    setConnected(false)
    setReconnecting(false)
  }, [])

  const connect = useCallback(async () => {
    disconnect()
    enabled.current = true
    attempts.current = 0
    await connectOnce()
  }, [connectOnce, disconnect])

  const send = useCallback((content: string, clientMsgId: string = crypto.randomUUID()) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return false
    try {
      ws.send(JSON.stringify({ type: 'chat', content, client_msg_id: clientMsgId }))
      return true
    } catch {
      return false
    }
  }, [])

  useEffect(() => () => {
    enabled.current = false
    generation.current++
    clearTimeout(timer.current)
    wsRef.current?.close()
  }, [])

  return { connected, reconnecting, connect, disconnect, send }
}
