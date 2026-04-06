import { useRef, useCallback, useEffect, useState } from 'react'
import type { WsOutgoing } from '../types'
import { createWsTicket } from '../api/client'

const MAX_RECONNECT_ATTEMPTS = 20
const BASE_DELAY_MS = 1000
const MAX_DELAY_MS = 30000

interface UseWebSocketOptions {
  roomId: string
  onMessage: (msg: WsOutgoing) => void
  onConflict?: () => void
  onReconnected?: () => void
  onGaveUp?: () => void
}

export function useWebSocket({ roomId, onMessage, onConflict, onReconnected, onGaveUp }: UseWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const attemptRef = useRef(0)
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined)
  const mountedRef = useRef(true)
  const shouldReconnectRef = useRef(true)

  const onMessageRef = useRef(onMessage)
  const onConflictRef = useRef(onConflict)
  const onReconnectedRef = useRef(onReconnected)
  const onGaveUpRef = useRef(onGaveUp)
  onMessageRef.current = onMessage
  onConflictRef.current = onConflict
  onReconnectedRef.current = onReconnected
  onGaveUpRef.current = onGaveUp

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

      ws.onopen = () => {
        if (!mountedRef.current) {
          ws.close()
          resolve(false)
          return
        }
        setConnected(true)
        const wasReconnect = attemptRef.current > 0
        attemptRef.current = 0
        if (wasReconnect) {
          onReconnectedRef.current?.()
        }
        resolve(true)
      }

      ws.onmessage = (ev) => {
        const msg: WsOutgoing = JSON.parse(ev.data)
        if (msg.type === 'conflict') {
          shouldReconnectRef.current = false
          onConflictRef.current?.()
          return
        }
        onMessageRef.current(msg)
      }

      ws.onerror = () => {
        resolve(false)
      }

      ws.onclose = () => {
        setConnected(false)
        wsRef.current = null
        if (mountedRef.current && shouldReconnectRef.current) {
          scheduleReconnect()
        }
      }
    })
  }, [roomId])

  const scheduleReconnect = useCallback(() => {
    if (!mountedRef.current || !shouldReconnectRef.current) return
    if (attemptRef.current >= MAX_RECONNECT_ATTEMPTS) {
      onGaveUpRef.current?.()
      return
    }

    attemptRef.current += 1
    const delay = Math.min(BASE_DELAY_MS * 2 ** (attemptRef.current - 1), MAX_DELAY_MS)

    timerRef.current = setTimeout(() => {
      if (mountedRef.current && shouldReconnectRef.current) {
        connectOnce()
      }
    }, delay)
  }, [connectOnce])

  const connect = useCallback(async () => {
    shouldReconnectRef.current = true
    attemptRef.current = 0
    if (wsRef.current) {
      wsRef.current.close()
    }
    await connectOnce()
  }, [connectOnce])

  const send = useCallback((content: string) => {
    if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) return
    const clientMsgId = crypto.randomUUID()
    wsRef.current.send(
      JSON.stringify({
        content,
        client_msg_id: clientMsgId,
        type: 'chat',
      }),
    )
  }, [])

  const disconnect = useCallback(() => {
    shouldReconnectRef.current = false
    clearTimeout(timerRef.current)
    wsRef.current?.close()
    wsRef.current = null
  }, [])

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      shouldReconnectRef.current = false
      clearTimeout(timerRef.current)
      wsRef.current?.close()
    }
  }, [])

  return { connected, connect, disconnect, send }
}
