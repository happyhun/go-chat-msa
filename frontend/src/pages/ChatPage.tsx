import { useState, useEffect, useRef, useLayoutEffect } from 'react'
import { Navigate, useParams, useNavigate, useLocation } from 'react-router-dom'
import { useAuth } from '../context/auth'
import { useChatRoom } from '../hooks/useChatRoom'
import RoomMembers from '../components/RoomMembers'
import ReconnectNotice from '../components/ReconnectNotice'
import ChatMessages from '../components/ChatMessages'

export default function ChatPage() {
  const { roomId } = useParams<{ roomId: string }>()
  const { userId } = useAuth()
  const location = useLocation()
  const [attempt, setAttempt] = useState(0)
  const state = location.state as { roomName?: string } | null
  if (!roomId) return <Navigate to="/rooms" replace />
  if (!userId) return <Navigate to="/login" replace />
  return <ChatRoom key={`${userId}:${roomId}:${attempt}`} onReload={() => setAttempt((value) => value + 1)} roomId={roomId} userId={userId} initialRoomName={state?.roomName ?? ''} />
}

function ChatRoom({ roomId, userId, initialRoomName, onReload }: { roomId: string; userId: string; initialRoomName: string; onReload: () => void }) {
  const navigate = useNavigate()
  const [input, setInput] = useState('')
  const [showMembers, setShowMembers] = useState(false)
  const bottomRef = useRef<HTMLDivElement>(null)
  const scrollContainerRef = useRef<HTMLDivElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const autoScrollRef = useRef(true)
  const {
    messages, disconnectReason, sendError, loading, loadError,
    userMap, memberIds, managerId, roomName, connected, exhausted,
    sendMessage, retryMessage, recover, disconnect, reconnect,
  } = useChatRoom(roomId, userId, initialRoomName)

  useEffect(() => {
    if (autoScrollRef.current) bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages])

  useLayoutEffect(() => {
    const textarea = textareaRef.current
    if (!textarea) return
    textarea.style.height = 'auto'
    textarea.style.height = `${textarea.scrollHeight}px`
  }, [input])

  const handleScroll = () => {
    const container = scrollContainerRef.current
    if (!container) return
    autoScrollRef.current = container.scrollHeight - container.scrollTop - container.clientHeight < 80
  }

  const handleSend = () => {
    if (sendMessage(input)) setInput('')
  }

  if (disconnectReason) {
    const info = (() => {
      switch (disconnectReason) {
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
                onClick={reconnect}
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

      {!connected && !loading && !loadError && <ReconnectNotice />}

      {showMembers && (
        <RoomMembers memberIds={memberIds} userMap={userMap} userId={userId} managerId={managerId} onClose={() => setShowMembers(false)} />
      )}

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
                  onClick={onReload}
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
            <ChatMessages messages={messages} userId={userId} userMap={userMap} retryMessage={retryMessage} />
          )}
          <div ref={bottomRef} />
        </div>
      </div>

      <div className="bg-white border-t border-gray-200 shrink-0">
        <div className="max-w-4xl mx-auto px-4 py-3">
          {sendError && (
            <div
              role="alert"
              aria-live="polite"
              className="mb-2 rounded-lg bg-red-50 px-3 py-2 text-xs text-red-700"
            >
              {sendError}
            </div>
          )}
          {exhausted && <button className="mb-2 text-xs text-amber-700 underline" onClick={() => recover()}>저장된 메시지 다시 확인</button>}
          <div className="flex items-end gap-2">
            <textarea
              ref={textareaRef}
              value={input}
              aria-label="메시지 입력"
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  handleSend()
                }
              }}
              maxLength={10000}
              rows={1}
              placeholder={
                loadError
                  ? '채팅방을 불러온 후 메시지를 보낼 수 있습니다'
                  : connected
                    ? '메시지를 입력하세요...'
                    : '연결을 기다리는 중입니다. 메시지를 미리 작성할 수 있습니다.'
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
