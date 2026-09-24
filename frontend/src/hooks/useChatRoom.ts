import { useState, useReducer, useEffect, useRef, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import { batchGetUsers, listMessages, listRoomMembers, listJoinedRooms, ApiError } from '../api/client'
import { useWebSocket, type WebSocketStopReason } from './useWebSocket'
import { useMessageSync } from './useMessageSync'
import { lastMessageId } from '../messages'
import { messageReducer } from '../messageState'
import type { MessageInfo, WsOutgoing } from '../types'

function toMessageInfo(msg: WsOutgoing | MessageInfo): MessageInfo {
  return {
    id: msg.id,
    room_id: msg.room_id,
    sender_id: msg.sender_id,
    content: msg.content,
    client_msg_id: msg.client_msg_id,
    type: msg.type,
    timestamp: msg.timestamp,
    status: 'status' in msg ? msg.status : undefined,
  }
}

export type DisconnectReason =
  | 'room_unavailable'
  | Exclude<WebSocketStopReason, 'auth'>
  | null

export function useChatRoom(roomId: string, userId: string, stateRoomName: string) {
  const navigate = useNavigate()
  const [messages, dispatch] = useReducer(messageReducer, [])
  const [disconnectReason, setDisconnectReason] = useState<DisconnectReason>(null)
  const [sendError, setSendError] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [userMap, setUserMap] = useState<Map<string, string>>(new Map())
  const userMapRef = useRef(userMap)
  const [memberIds, setMemberIds] = useState<string[]>([])
  const [managerId, setManagerId] = useState<string | null>(null)
  const [roomName, setRoomName] = useState(stateRoomName)
  const lastIdRef = useRef<string>('')
  const pendingCursors = useRef(new Map<string, string>())

  useEffect(() => {
    const timer = setInterval(() => {
      dispatch({ type: 'timeout', now: Date.now() / 1000 })
    }, 1000)
    return () => clearInterval(timer)
  }, [])

  const updateLastId = useCallback((msgs: MessageInfo[]) => {
    const last = lastMessageId(msgs)
    if (last > lastIdRef.current) lastIdRef.current = last
  }, [])

  const fetchMembers = useCallback(async () => {
    if (!roomId) return
    try {
      const data = await listRoomMembers(roomId)
      const members = data.members ?? []
      setMemberIds(members.map((m) => m.user_id))
      setUserMap((prev) => {
        const next = new Map(prev)
        for (const m of members) next.set(m.user_id, m.username)
        return next
      })
    } catch {
      return
    }
  }, [roomId])

  const ensureSendersLoaded = useCallback(async (msgs: MessageInfo[]) => {
    const senderIds = [
      ...new Set(msgs.filter((m) => m.type === 'chat').map((m) => m.sender_id)),
    ]
    const missing = senderIds.filter((id) => !userMapRef.current.has(id))
    if (missing.length === 0) return
    try {
      const users = await batchGetUsers(missing)
      if (users.length === 0) return
      setUserMap((prev) => {
        const next = new Map(prev)
        for (const u of users) next.set(u.user_id, u.username)
        return next
      })
    } catch (err) {
      console.warn('batchGetUsers failed:', err)
    }
  }, [])

  useEffect(() => {
    userMapRef.current = userMap
  }, [userMap])

  const fetchRoomInfo = useCallback(async () => {
    if (!roomId) return
    try {
      const data = await listJoinedRooms()
      const room = (data.rooms ?? []).find((r) => r.id === roomId)
      if (room) {
        if (!stateRoomName) setRoomName(room.name)
        setManagerId(room.manager_id)
      }
    } catch {
      return
    }
  }, [roomId, stateRoomName])

  const onMessage = useCallback((msg: WsOutgoing | MessageInfo) => {
    const m = toMessageInfo(msg)
    if (!m.status && m.sender_id === userId && m.room_id === roomId && m.client_msg_id) {
      pendingCursors.current.delete(m.client_msg_id)
    }
    if (m.type === 'system') {
      fetchMembers()
    } else if (!userMapRef.current.has(m.sender_id)) {
      ensureSendersLoaded([m])
    }
    updateLastId([m])
    dispatch({ type: 'received', message: m })
  }, [fetchMembers, ensureSendersLoaded, updateLastId, userId, roomId])

  const onSyncMessages = useCallback((incoming: MessageInfo[]) => {
    for (const message of incoming) {
      if (message.sender_id === userId && message.room_id === roomId && message.client_msg_id) {
        pendingCursors.current.delete(message.client_msg_id)
      }
    }
    ensureSendersLoaded(incoming)
    updateLastId(incoming)
    dispatch({ type: 'synced', messages: incoming })
  }, [ensureSendersLoaded, updateLastId, userId, roomId])
  const { recover, exhausted } = useMessageSync(roomId, userId, lastIdRef, onSyncMessages)

  const onConnected = useCallback(() => {
    recover()
    fetchMembers()
  }, [recover, fetchMembers])

  const onGaveUp = useCallback(async (reason: WebSocketStopReason) => {
    if (reason === 'auth') {
      navigate('/login', { replace: true })
      return
    }

    if (reason !== 'connection_failed' || !roomId) {
      setDisconnectReason(reason)
      return
    }

    try {
      const data = await listJoinedRooms()
      const stillJoined = (data.rooms ?? []).some((room) => room.id === roomId)
      setDisconnectReason(stillJoined ? reason : 'room_unavailable')
    } catch {
      setDisconnectReason(reason)
    }
  }, [navigate, roomId])

  const { connected, connect, disconnect, send } = useWebSocket({
    roomId,
    onMessage,
    onConnected,
    onGaveUp,
  })

  useEffect(() => {
    if (!roomId) return

    let cancelled = false
    async function init() {
      try {
        const [msgData] = await Promise.all([
          listMessages(roomId).catch((error: unknown) => {
            if (error instanceof ApiError && error.status === 503) return { messages: [], has_more: false }
            throw error
          }),
          fetchMembers(),
          fetchRoomInfo(),
        ])
        if (cancelled) return
        const msgs = msgData.messages ?? []
        ensureSendersLoaded(msgs)
        dispatch({ type: 'loaded', messages: msgs })
        updateLastId(msgs)
        setLoading(false)

        await connect()
      } catch (err) {
        if (cancelled) return
        setLoading(false)
        if (err instanceof ApiError && err.status === 401) {
          navigate('/login')
          return
        }
        setLoadError(
          err instanceof ApiError
            ? err.message
            : '채팅방 정보를 불러오지 못했습니다. 잠시 후 다시 시도해 주세요.',
        )
      }
    }

    init()

    return () => {
      cancelled = true
      disconnect()
    }
  }, [roomId, fetchMembers, fetchRoomInfo, ensureSendersLoaded, updateLastId, connect, disconnect, navigate])

  const sendMessage = (content: string) => {
    const text = content.trim()
    if (!text || loadError) return false
    const clientMsgId = crypto.randomUUID()
    if (!send(text, clientMsgId)) {
      setSendError('연결이 복구된 뒤 다시 보내 주세요.')
      return false
    }
    pendingCursors.current.set(clientMsgId, lastIdRef.current)
    onMessage({
      id: `pending:${clientMsgId}`,
      room_id: roomId,
      sender_id: userId,
      content: text,
      client_msg_id: clientMsgId,
      type: 'chat',
      timestamp: Date.now() / 1000,
      status: 'sending',
    })
    setSendError('')
    return true
  }

  const retryMessage = (message: MessageInfo) => {
    if (!message.client_msg_id || !send(message.content, message.client_msg_id)) {
      setSendError('연결이 복구된 뒤 다시 보내 주세요.')
      return
    }
    dispatch({ type: 'retried', message, now: Date.now() / 1000 })
    setSendError('')
    recover(pendingCursors.current.get(message.client_msg_id) ?? '')
  }

  const reconnect = () => {
    setDisconnectReason(null)
    void connect()
  }

  return {
    messages, disconnectReason, sendError, loading, loadError,
    userMap, memberIds, managerId, roomName, connected, exhausted,
    sendMessage, retryMessage, recover, disconnect, reconnect,
  }
}
