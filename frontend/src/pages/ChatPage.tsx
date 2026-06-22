import { useState, useEffect, useRef, useCallback, useLayoutEffect, useId } from 'react'
import { useParams, useNavigate, useLocation } from 'react-router-dom'
import { batchGetUsers, listMessages, listRoomMembers, listJoinedRooms, ApiError } from '../api/client'
import { useAuth } from '../context/auth'
import { useWebSocket, type WebSocketStopReason } from '../hooks/useWebSocket'
import type { MessageInfo, WsOutgoing } from '../types'

function formatTime(unix: number) {
  const d = new Date(unix * 1000)
  return d.toLocaleTimeString('ko-KR', { hour: '2-digit', minute: '2-digit' })
}

function insertSorted(prev: MessageInfo[], msg: MessageInfo): MessageInfo[] {
  if (prev.some((m) => isSameMessage(m, msg))) return prev
  if (prev.length === 0 || msg.sequence_number >= prev[prev.length - 1].sequence_number) {
    return [...prev, msg]
  }
  let lo = 0
  let hi = prev.length
  while (lo < hi) {
    const mid = (lo + hi) >>> 1
    if (prev[mid].sequence_number < msg.sequence_number) lo = mid + 1
    else hi = mid
  }
  const result = [...prev]
  result.splice(lo, 0, msg)
  return result
}

function isSameMessage(a: MessageInfo, b: MessageInfo): boolean {
  if (a.id && b.id && a.id === b.id) return true
  return Boolean(a.client_msg_id && b.client_msg_id && a.client_msg_id === b.client_msg_id)
}

function mergeSorted(prev: MessageInfo[], batch: MessageInfo[]): MessageInfo[] {
  if (batch.length === 0) return prev
  const ids = new Set(prev.map((m) => m.id).filter(Boolean))
  const clientMsgIds = new Set(prev.map((m) => m.client_msg_id).filter(Boolean))
  const newOnes = batch.filter((m) => {
    if (m.id && ids.has(m.id)) return false
    if (m.client_msg_id && clientMsgIds.has(m.client_msg_id)) return false
    return true
  })
  if (newOnes.length === 0) return prev
  return [...prev, ...newOnes].sort((a, b) => a.sequence_number - b.sequence_number)
}

function toMessageInfo(msg: WsOutgoing): MessageInfo {
  return {
    id: msg.id,
    room_id: msg.room_id,
    sender_id: msg.sender_id,
    content: msg.content,
    client_msg_id: msg.client_msg_id,
    type: msg.type,
    sequence_number: msg.sequence_number,
    timestamp: msg.timestamp,
  }
}

const SYNC_GAP_RETRY_ATTEMPTS = 3
const SYNC_GAP_RETRY_MIN_MS = 500
const SYNC_GAP_RETRY_MAX_MS = 3500

type DisconnectReason =
  | 'conflict'
  | 'room_unavailable'
  | Exclude<WebSocketStopReason, 'auth'>
  | null

function syncRetryDelayMs() {
  return SYNC_GAP_RETRY_MIN_MS + Math.random() * (SYNC_GAP_RETRY_MAX_MS - SYNC_GAP_RETRY_MIN_MS)
}

function delay(ms: number) {
  return new Promise((resolve) => window.setTimeout(resolve, ms))
}

export default function ChatPage() {
  const { roomId } = useParams<{ roomId: string }>()
  const navigate = useNavigate()
  const location = useLocation()
  const { userId } = useAuth()
  const membersTitleId = useId()

  const stateRoomName = (location.state as { roomName?: string } | null)?.roomName ?? ''

  const [messages, setMessages] = useState<MessageInfo[]>([])
  const [input, setInput] = useState('')
  const [disconnectReason, setDisconnectReason] = useState<DisconnectReason>(null)
  const [reconnectNotice, setReconnectNotice] = useState<'short' | 'long' | null>(null)
  const [sendError, setSendError] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [reloadKey, setReloadKey] = useState(0)
  const [userMap, setUserMap] = useState<Map<string, string>>(new Map())
  const userMapRef = useRef(userMap)
  const [memberIds, setMemberIds] = useState<string[]>([])
  const [managerId, setManagerId] = useState<string | null>(null)
  const [roomName, setRoomName] = useState(stateRoomName)
  const [showMembers, setShowMembers] = useState(false)
  const bottomRef = useRef<HTMLDivElement>(null)
  const scrollContainerRef = useRef<HTMLDivElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const maxSeqRef = useRef<number>(0)
  const lastSyncRef = useRef<number>(0)
  const autoScrollRef = useRef(true)

  const updateMaxSeq = (msgs: MessageInfo[]) => {
    if (msgs.length === 0) return
    const last = msgs[msgs.length - 1].sequence_number
    if (last > maxSeqRef.current) maxSeqRef.current = last
  }

  const handleScroll = () => {
    const el = scrollContainerRef.current
    if (!el) return
    autoScrollRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  }

  const scrollToBottom = useCallback(() => {
    if (autoScrollRef.current) {
      bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
    }
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
      // non-critical
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
      // non-critical
    }
  }, [roomId, stateRoomName])

  const syncMissed = useCallback(async (
    required = false,
    fromSeq = maxSeqRef.current,
    retryGap = false,
  ): Promise<Set<string>> => {
    const caughtUpClientMsgIds = new Set<string>()
    if (!roomId) return caughtUpClientMsgIds
    const expectedSeq = fromSeq + 1
    const attempts = retryGap ? SYNC_GAP_RETRY_ATTEMPTS + 1 : 1

    for (let attempt = 0; attempt < attempts; attempt++) {
      try {
        const data = await listMessages(roomId, fromSeq)
        const syncMsgs = (data.messages ?? []) as MessageInfo[]
        for (const msg of syncMsgs) {
          if (msg.client_msg_id) caughtUpClientMsgIds.add(msg.client_msg_id)
        }
        if (syncMsgs.length > 0) {
          ensureSendersLoaded(syncMsgs)
          setMessages((prev) => {
            const merged = mergeSorted(prev, syncMsgs)
            updateMaxSeq(merged)
            return merged
          })
        }
        if (!retryGap || syncMsgs.some((msg) => msg.sequence_number === expectedSeq)) {
          break
        }
      } catch (err) {
        if (required && attempt + 1 >= attempts) throw err
      }

      if (attempt + 1 < attempts) {
        await delay(syncRetryDelayMs())
      }
    }
    return caughtUpClientMsgIds
  }, [roomId, ensureSendersLoaded])

  const syncMissedThrottled = useCallback((fromSeq = maxSeqRef.current) => {
    const now = Date.now()
    if (now - lastSyncRef.current < 1000) return
    lastSyncRef.current = now
    syncMissed(false, fromSeq, true)
  }, [syncMissed])

  const onMessage = useCallback((msg: WsOutgoing) => {
    const m = toMessageInfo(msg)
    if (m.type === 'system') {
      fetchMembers()
    } else {
      const lastSeenSeq = maxSeqRef.current
      if (lastSeenSeq > 0 && m.sequence_number > lastSeenSeq + 1) {
        syncMissedThrottled(lastSeenSeq)
      }
      if (!userMapRef.current.has(m.sender_id)) {
        ensureSendersLoaded([m])
      }
    }
    setMessages((prev) => {
      const next = insertSorted(prev, m)
      if (next !== prev && m.sequence_number > maxSeqRef.current) {
        maxSeqRef.current = m.sequence_number
      }
      return next
    })
  }, [fetchMembers, ensureSendersLoaded, syncMissedThrottled])

  const onConflict = useCallback(() => {
    setDisconnectReason('conflict')
  }, [])

  const onReconnected = useCallback(async () => {
    const caughtUpClientMsgIds = await syncMissed(true)
    fetchMembers()
    return caughtUpClientMsgIds
  }, [syncMissed, fetchMembers])

  const onGaveUp = useCallback((reason: WebSocketStopReason) => {
    if (reason === 'auth') {
      navigate('/login', { replace: true })
      return
    }

    if (reason === 'connection_failed' || reason === 'service_unavailable' || reason === 'ticket_failed') {
      void (async () => {
        if (!roomId) {
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
      })()
      return
    }

    setDisconnectReason(reason)
  }, [navigate, roomId])

  const { connected, reconnecting, queuedCount, connect, disconnect, send } = useWebSocket({
    roomId: roomId!,
    onMessage,
    onConflict,
    onReconnected,
    onGaveUp,
  })

  useEffect(() => {
    if (!roomId) return

    let cancelled = false
    setMessages([])
    setInput('')
    setDisconnectReason(null)
    setReconnectNotice(null)
    setSendError('')
    setLoadError('')
    setLoading(true)
    setMemberIds([])
    setManagerId(null)
    setRoomName(stateRoomName)
    maxSeqRef.current = 0
    lastSyncRef.current = 0
    autoScrollRef.current = true

    async function init() {
      try {
        const [msgData] = await Promise.all([
          listMessages(roomId!),
          fetchMembers(),
          fetchRoomInfo(),
        ])
        if (cancelled) return
        const msgs = (msgData.messages ?? []).sort(
          (a, b) => a.sequence_number - b.sequence_number,
        )
        ensureSendersLoaded(msgs)
        setMessages(msgs)
        updateMaxSeq(msgs)
        setLoading(false)

        await connect()
        if (cancelled) return

        if (maxSeqRef.current > 0) {
          try {
            const sync = await listMessages(roomId!, maxSeqRef.current)
            if (cancelled) return
            const syncMsgs = (sync.messages ?? []) as MessageInfo[]
            if (syncMsgs.length > 0) {
              ensureSendersLoaded(syncMsgs)
              setMessages((prev) => {
                const merged = mergeSorted(prev, syncMsgs)
                updateMaxSeq(merged)
                return merged
              })
            }
          } catch {
            // non-critical
          }
        }
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [roomId, reloadKey])

  useEffect(() => {
    scrollToBottom()
  }, [messages, scrollToBottom])

  useEffect(() => {
    if (connected || disconnectReason || loading || loadError) {
      setReconnectNotice(null)
      return
    }
    const shortTimer = window.setTimeout(() => setReconnectNotice('short'), 300)
    const longTimer = window.setTimeout(() => setReconnectNotice('long'), 10000)
    return () => {
      window.clearTimeout(shortTimer)
      window.clearTimeout(longTimer)
    }
  }, [connected, reconnecting, disconnectReason, loading, loadError])

  useEffect(() => {
    if (queuedCount === 0) setSendError('')
  }, [queuedCount])

  useEffect(() => {
    if (!showMembers) return
    const handleKey = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') setShowMembers(false)
    }
    document.addEventListener('keydown', handleKey)
    return () => document.removeEventListener('keydown', handleKey)
  }, [showMembers])

  // Auto-resize textarea
  useLayoutEffect(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight}px`
  }, [input])

  const handleSend = (e?: React.SyntheticEvent) => {
    e?.preventDefault()
    const text = input.trim()
    if (!text || loadError) return
    const result = send(text)
    if (!result.ok) {
      setSendError('전송 대기 메시지가 너무 많습니다. 연결이 복구된 뒤 다시 보내 주세요.')
      return
    }
    setSendError('')
    setInput('')
  }

  const getUsername = (senderId: string) => userMap.get(senderId) ?? '(탈퇴한 사용자)'

  if (disconnectReason) {
    const info = (() => {
      switch (disconnectReason) {
        case 'conflict':
          return {
            title: '연결 충돌',
            desc: '다른 탭에서 같은 채팅방에 접속하여 이 연결이 종료되었습니다.',
            retry: false,
          }
        case 'room_unavailable':
          return {
            title: '채팅방을 이용할 수 없습니다',
            desc: '채팅방이 삭제되었거나 더 이상 참여 중인 방이 아닙니다.',
            retry: false,
          }
        case 'rate_limited':
          return {
            title: '연결 요청이 너무 많습니다',
            desc: '잠시 후 다시 연결해 주세요.',
            retry: true,
          }
        case 'service_unavailable':
          return {
            title: '채팅 서버가 일시적으로 불안정합니다',
            desc: '서버 상태가 회복되면 다시 연결할 수 있습니다.',
            retry: true,
          }
        default:
          return {
            title: '채팅방에 연결하지 못했습니다',
            desc: '네트워크 상태를 확인한 뒤 다시 시도해 주세요.',
            retry: true,
          }
      }
    })()

    return (
      <div className="min-h-screen flex items-center justify-center bg-gray-50 px-4">
        <div className="bg-white rounded-lg shadow-sm border border-gray-100 p-8 text-center max-w-sm">
          <p className="text-gray-900 font-medium mb-2">{info.title}</p>
          <p className="text-sm text-gray-500 mb-4">{info.desc}</p>
          <div className="flex justify-center gap-2">
            {info.retry && (
              <button
                onClick={() => {
                  setDisconnectReason(null)
                  setReconnectNotice(null)
                  void connect()
                }}
                className="px-4 py-2.5 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 transition-colors"
              >
                다시 연결
              </button>
            )}
            <button
              onClick={() => navigate('/rooms', { replace: true })}
              className="px-4 py-2.5 border border-gray-300 text-gray-700 rounded-lg text-sm font-medium hover:bg-gray-50 transition-colors"
            >
              채팅방 목록으로
            </button>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="h-screen flex flex-col bg-gray-50">
      {/* Header */}
      <header className="bg-white border-b border-gray-200 shrink-0">
        <div className="max-w-4xl mx-auto px-4 h-14 flex items-center gap-3">
          <button
            onClick={() => {
              disconnect()
              navigate(-1)
            }}
            className="text-gray-500 hover:text-gray-900 text-sm shrink-0"
          >
            &larr;
          </button>
          <div className="min-w-0 flex-1">
            <span className="font-semibold text-gray-900 text-sm truncate block">
              {roomName || '채팅방'}
            </span>
            {memberIds.length > 0 && (
              <button
                onClick={() => setShowMembers((v) => !v)}
                className="text-xs text-gray-500 hover:text-indigo-600 transition-colors"
              >
                {memberIds.length}명 참여 중
              </button>
            )}
          </div>
          <div className="flex items-center gap-1.5 shrink-0">
            <span
              className={`w-2 h-2 rounded-full ${
                connected ? 'bg-green-400' : loadError ? 'bg-red-400' : 'bg-amber-400 animate-pulse'
              }`}
            />
            <span className="text-xs text-gray-400">
              {connected ? '연결됨' : loadError ? '연결 안 됨' : '재연결 중...'}
            </span>
          </div>
        </div>
      </header>

      {reconnectNotice && (
        <div className="bg-amber-50 border-b border-amber-100 text-amber-800 text-xs">
          <div className="max-w-4xl mx-auto px-4 py-2">
            {reconnectNotice === 'long'
              ? '연결 복구가 지연되고 있습니다. 보낸 메시지는 재연결 후 전송됩니다.'
              : '채팅방 재접속 중입니다'}
          </div>
        </div>
      )}

      {/* Member drawer */}
      {showMembers && (
        <div className="fixed inset-0 z-50 flex justify-end" onClick={() => setShowMembers(false)}>
          <div className="absolute inset-0 bg-black/30" />
          <div
            role="dialog"
            aria-modal="true"
            aria-labelledby={membersTitleId}
            className="relative w-72 max-w-[80vw] bg-white h-full shadow-xl flex flex-col animate-slide-in"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="px-4 py-4 border-b border-gray-200 flex items-center justify-between">
              <span id={membersTitleId} className="text-sm font-semibold text-gray-900">멤버 ({memberIds.length})</span>
              <button
                type="button"
                onClick={() => setShowMembers(false)}
                className="text-gray-400 hover:text-gray-600"
                aria-label="멤버 목록 닫기"
              >
                <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <div className="flex-1 overflow-y-auto">
              {memberIds.map((id) => (
                <div key={id} className="px-4 py-3 flex items-center justify-between border-b border-gray-50">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="text-sm text-gray-900 truncate">{userMap.get(id) ?? '(알 수 없음)'}</span>
                    {id === userId && (
                      <span className="text-xs text-indigo-600 bg-indigo-50 px-2 py-0.5 rounded font-medium shrink-0">나</span>
                    )}
                  </div>
                  {id === managerId && (
                    <span className="text-xs text-amber-600 bg-amber-50 px-2 py-0.5 rounded font-medium shrink-0">방장</span>
                  )}
                </div>
              ))}
            </div>
          </div>
        </div>
      )}

      {/* Messages */}
      <div ref={scrollContainerRef} onScroll={handleScroll} className="flex-1 overflow-y-auto overscroll-contain bg-gray-50">
        <div className="max-w-4xl mx-auto px-4 py-4">
          {!loadError && !loading && (
            <div className="mb-3 text-center">
              <span className="inline-flex rounded-full bg-gray-100 px-3 py-1 text-xs text-gray-500">
                참여 이후 메시지만 표시됩니다.
              </span>
            </div>
          )}
          {loadError ? (
            <div className="py-16 text-center">
              <p className="text-sm font-medium text-gray-900">채팅방을 불러오지 못했습니다.</p>
              <p className="mt-1 text-sm text-gray-500">{loadError}</p>
              <div className="mt-4 flex justify-center gap-2">
                <button
                  type="button"
                  onClick={() => setReloadKey((value) => value + 1)}
                  className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 transition-colors"
                >
                  다시 시도
                </button>
                <button
                  type="button"
                  onClick={() => navigate('/rooms')}
                  className="px-4 py-2 border border-gray-300 text-gray-700 rounded-lg text-sm hover:bg-gray-50 transition-colors"
                >
                  채팅방 목록으로
                </button>
              </div>
            </div>
          ) : loading ? (
            <p className="text-center text-sm text-gray-400 py-8">메시지를 불러오는 중...</p>
          ) : messages.length === 0 ? (
            <p className="text-center text-sm text-gray-400 py-8">
              아직 메시지가 없습니다. 첫 메시지를 보내보세요!
            </p>
          ) : (
            messages.map((msg, i) => {
              if (msg.type === 'system') {
                return (
                  <div key={msg.id} className="text-center py-2">
                    <span className="text-xs text-gray-400 bg-gray-100 px-3 py-1 rounded-full">
                      {msg.content}
                    </span>
                  </div>
                )
              }

              const isMine = msg.sender_id === userId
              const prev = messages[i - 1]
              const next = messages[i + 1]
              const sameSenderAsPrev =
                prev && prev.type !== 'system' && prev.sender_id === msg.sender_id
              const sameSenderAsNext =
                next && next.type !== 'system' && next.sender_id === msg.sender_id
              const showName = !isMine && !sameSenderAsPrev
              const showTime = !sameSenderAsNext
              const senderName = getUsername(msg.sender_id)

              return (
                <div
                  key={msg.id}
                  className={`flex ${isMine ? 'justify-end' : 'justify-start'} ${sameSenderAsPrev ? 'mt-0.5' : 'mt-3'}`}
                >
                  <div
                    className={`flex flex-col ${isMine ? 'items-end' : 'items-start'} max-w-[70%]`}
                  >
                    {showName && senderName && (
                      <span className="text-xs text-gray-500 mb-0.5 px-1">{senderName}</span>
                    )}
                    <div
                      className={`px-3.5 py-2 text-sm break-words whitespace-pre-wrap ${
                        isMine
                          ? `bg-indigo-600 text-white ${sameSenderAsPrev ? 'rounded-2xl rounded-tr-md' : 'rounded-2xl'} ${sameSenderAsNext ? 'rounded-br-md' : ''}`
                          : `bg-white text-gray-900 border border-gray-100 ${sameSenderAsPrev ? 'rounded-2xl rounded-tl-md' : 'rounded-2xl'} ${sameSenderAsNext ? 'rounded-bl-md' : ''}`
                      }`}
                    >
                      {msg.content}
                    </div>
                    {showTime && (
                      <span className="text-xs text-gray-400 mt-0.5 px-1">
                        {formatTime(msg.timestamp)}
                      </span>
                    )}
                  </div>
                </div>
              )
            })
          )}
          <div ref={bottomRef} />
        </div>
      </div>

      {/* Input */}
      <div className="bg-white border-t border-gray-200 shrink-0">
        <div className="max-w-4xl mx-auto px-4 py-3">
          {(queuedCount > 0 || sendError) && (
            <div
              role={sendError ? 'alert' : 'status'}
              aria-live="polite"
              className={`mb-2 rounded-lg px-3 py-2 text-xs ${
                sendError
                  ? 'bg-red-50 text-red-700'
                  : 'bg-amber-50 text-amber-800'
              }`}
            >
              {sendError || `전송 대기 중인 메시지 ${queuedCount}개가 있습니다. 연결이 복구되면 자동으로 전송됩니다.`}
            </div>
          )}
          <div className="flex items-end gap-2">
            <textarea
              ref={textareaRef}
              value={input}
              aria-label="메시지 입력"
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  handleSend(e)
                }
              }}
              maxLength={10000}
              rows={1}
              placeholder={
                loadError
                  ? '채팅방을 불러온 후 메시지를 보낼 수 있습니다'
                  : connected
                    ? '메시지를 입력하세요...'
                    : '재연결 후 전송 대기열에 추가됩니다...'
              }
              disabled={Boolean(loadError)}
              className="flex-1 px-4 py-2.5 bg-gray-100 rounded-2xl text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500 resize-none overflow-y-auto leading-5"
              style={{ maxHeight: '120px' }}
              autoFocus
            />
            <button
              type="button"
              aria-label="메시지 보내기"
              onClick={handleSend}
              disabled={!input.trim() || Boolean(loadError)}
              className="w-10 h-10 bg-indigo-600 text-white rounded-full flex items-center justify-center hover:bg-indigo-700 disabled:opacity-30 transition-colors shrink-0"
            >
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  strokeWidth={2}
                  d="M5 12h14M12 5l7 7-7 7"
                />
              </svg>
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
