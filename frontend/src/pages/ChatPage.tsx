import { useState, useEffect, useRef, useCallback, useLayoutEffect } from 'react'
import { useParams, useNavigate, useLocation } from 'react-router-dom'
import { listMessages, listRoomMembers, listJoinedRooms, ApiError } from '../api/client'
import { useAuth } from '../context/AuthContext'
import { useWebSocket } from '../hooks/useWebSocket'
import type { MessageInfo, WsOutgoing } from '../types'

function formatTime(unix: number) {
  const d = new Date(unix * 1000)
  return d.toLocaleTimeString('ko-KR', { hour: '2-digit', minute: '2-digit' })
}

function insertSorted(prev: MessageInfo[], msg: MessageInfo): MessageInfo[] {
  if (prev.some((m) => m.id === msg.id)) return prev
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

function mergeSorted(prev: MessageInfo[], batch: MessageInfo[]): MessageInfo[] {
  if (batch.length === 0) return prev
  const ids = new Set(prev.map((m) => m.id))
  const newOnes = batch.filter((m) => !ids.has(m.id))
  if (newOnes.length === 0) return prev
  return [...prev, ...newOnes].sort((a, b) => a.sequence_number - b.sequence_number)
}

function toMessageInfo(msg: WsOutgoing): MessageInfo {
  return {
    id: msg.id,
    room_id: msg.room_id,
    sender_id: msg.sender_id,
    content: msg.content,
    type: msg.type,
    timestamp: msg.timestamp,
    sequence_number: msg.sequence_number,
  }
}

type DisconnectReason = 'conflict' | 'room_deleted' | null

export default function ChatPage() {
  const { roomId } = useParams<{ roomId: string }>()
  const navigate = useNavigate()
  const location = useLocation()
  const { userId } = useAuth()

  const stateRoomName = (location.state as { roomName?: string } | null)?.roomName ?? ''

  const [messages, setMessages] = useState<MessageInfo[]>([])
  const [input, setInput] = useState('')
  const [disconnectReason, setDisconnectReason] = useState<DisconnectReason>(null)
  const [loading, setLoading] = useState(true)
  const [memberMap, setMemberMap] = useState<Map<string, string>>(new Map())
  const [managerId, setManagerId] = useState<string | null>(null)
  const [roomName, setRoomName] = useState(stateRoomName)
  const [showMembers, setShowMembers] = useState(false)
  const bottomRef = useRef<HTMLDivElement>(null)
  const scrollContainerRef = useRef<HTMLDivElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const maxSeqRef = useRef<number>(0)
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
      const map = new Map<string, string>()
      for (const m of data.members ?? []) {
        map.set(m.user_id, m.username)
      }
      setMemberMap(map)
    } catch {
      // non-critical
    }
  }, [roomId])

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

  const syncMissed = useCallback(async () => {
    if (!roomId || maxSeqRef.current === 0) return
    try {
      const data = await listMessages(roomId, maxSeqRef.current)
      const syncMsgs = (data.messages ?? []) as MessageInfo[]
      if (syncMsgs.length > 0) {
        setMessages((prev) => {
          const merged = mergeSorted(prev, syncMsgs)
          updateMaxSeq(merged)
          return merged
        })
      }
    } catch {
      // non-critical
    }
  }, [roomId])

  const onMessage = useCallback((msg: WsOutgoing) => {
    const m = toMessageInfo(msg)
    if (m.type === 'system') {
      fetchMembers()
    }
    setMessages((prev) => {
      const next = insertSorted(prev, m)
      if (next !== prev && m.sequence_number > maxSeqRef.current) {
        maxSeqRef.current = m.sequence_number
      }
      return next
    })
  }, [fetchMembers])

  const onConflict = useCallback(() => {
    setDisconnectReason('conflict')
  }, [])

  const onReconnected = useCallback(() => {
    syncMissed()
    fetchMembers()
  }, [syncMissed, fetchMembers])

  const onGaveUp = useCallback(() => {
    setDisconnectReason('room_deleted')
  }, [])

  const { connected, connect, disconnect, send } = useWebSocket({
    roomId: roomId!,
    onMessage,
    onConflict,
    onReconnected,
    onGaveUp,
  })

  useEffect(() => {
    if (!roomId) return

    let cancelled = false

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
        }
      }
    }

    init()

    return () => {
      cancelled = true
      disconnect()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [roomId])

  useEffect(() => {
    scrollToBottom()
  }, [messages, scrollToBottom])

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
    if (!text || !connected) return
    send(text)
    setInput('')
  }

  const getUsername = (senderId: string) => memberMap.get(senderId)

  if (disconnectReason) {
    const info = disconnectReason === 'conflict'
      ? { title: '연결 충돌', desc: '다른 탭에서 같은 채팅방에 접속하여 연결이 끊어졌습니다.' }
      : { title: '채팅방 삭제됨', desc: '이 채팅방은 방장에 의해 삭제되었습니다.' }

    return (
      <div className="min-h-screen flex items-center justify-center bg-gray-50 px-4">
        <div className="bg-white rounded-2xl shadow-sm border border-gray-100 p-8 text-center max-w-sm">
          <p className="text-gray-900 font-medium mb-2">{info.title}</p>
          <p className="text-sm text-gray-500 mb-4">{info.desc}</p>
          <button
            onClick={() => navigate('/rooms', { replace: true })}
            className="px-4 py-2.5 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 transition-colors"
          >
            채팅방 목록으로
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="h-screen flex flex-col bg-gray-50">
      {/* Header */}
      <header className="bg-white border-b border-gray-200 shrink-0">
        <div className="max-w-2xl mx-auto px-4 h-14 flex items-center gap-3">
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
            {memberMap.size > 0 && (
              <button
                onClick={() => setShowMembers((v) => !v)}
                className="text-[11px] text-gray-400 hover:text-indigo-600 transition-colors"
              >
                {memberMap.size}명 참여 중
              </button>
            )}
          </div>
          <div className="flex items-center gap-1.5 shrink-0">
            <span
              className={`w-2 h-2 rounded-full ${connected ? 'bg-green-400' : 'bg-amber-400 animate-pulse'}`}
            />
            <span className="text-xs text-gray-400">
              {connected ? '연결됨' : '재연결 중...'}
            </span>
          </div>
        </div>
      </header>

      {/* Member drawer */}
      {showMembers && (
        <div className="fixed inset-0 z-50 flex justify-end" onClick={() => setShowMembers(false)}>
          <div className="absolute inset-0 bg-black/30" />
          <div
            className="relative w-72 max-w-[80vw] bg-white h-full shadow-xl flex flex-col animate-slide-in"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="px-4 py-4 border-b border-gray-200 flex items-center justify-between">
              <span className="text-sm font-semibold text-gray-900">멤버 ({memberMap.size})</span>
              <button onClick={() => setShowMembers(false)} className="text-gray-400 hover:text-gray-600">
                <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <div className="flex-1 overflow-y-auto">
              {Array.from(memberMap.entries()).map(([id, name]) => (
                <div key={id} className="px-4 py-3 flex items-center justify-between border-b border-gray-50">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="text-sm text-gray-900 truncate">{name}</span>
                    {id === userId && (
                      <span className="text-[10px] text-indigo-600 bg-indigo-50 px-1.5 py-0.5 rounded font-medium shrink-0">나</span>
                    )}
                  </div>
                  {id === managerId && (
                    <span className="text-[10px] text-amber-600 bg-amber-50 px-1.5 py-0.5 rounded font-medium shrink-0">방장</span>
                  )}
                </div>
              ))}
            </div>
          </div>
        </div>
      )}

      {/* Messages */}
      <div ref={scrollContainerRef} onScroll={handleScroll} className="flex-1 overflow-y-auto overscroll-contain bg-gray-50">
        <div className="max-w-2xl mx-auto px-4 py-4">
          {loading ? (
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
                      <span className="text-[11px] text-gray-500 mb-0.5 px-1">{senderName}</span>
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
                      <span className="text-[10px] text-gray-400 mt-0.5 px-1">
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
        <div className="max-w-2xl mx-auto px-4 py-3 flex items-end gap-2">
          <textarea
            ref={textareaRef}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault()
                handleSend(e)
              }
            }}
            maxLength={10000}
            rows={1}
            placeholder={connected ? '메시지를 입력하세요...' : '재연결 중...'}
            disabled={!connected}
            className="flex-1 px-4 py-2.5 bg-gray-100 rounded-2xl text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500 disabled:opacity-50 resize-none overflow-y-auto leading-5"
            style={{ maxHeight: '120px' }}
            autoFocus
          />
          <button
            type="button"
            onClick={handleSend}
            disabled={!connected || !input.trim()}
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
  )
}
