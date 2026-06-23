import { useState, useEffect, useCallback, useId, useRef, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { listJoinedRooms, updateRoom, deleteRoom, leaveRoom, ApiError } from '../api/client'
import { useAuth } from '../context/auth'
import { useToast } from '../context/toast'
import type { RoomInfo } from '../types'
import ErrorBanner from '../components/ErrorBanner'
import EmptyState from '../components/EmptyState'
import CreateRoomModal from '../components/CreateRoomModal'
import ConfirmModal from '../components/ConfirmModal'

function EditRoomModal({
  room,
  onClose,
  onUpdated,
}: {
  room: RoomInfo
  onClose: () => void
  onUpdated: () => void
}) {
  const titleId = useId()
  const errorId = useId()
  const nameId = useId()
  const capacityId = useId()
  const dialogRef = useRef<HTMLDivElement>(null)
  const nameRef = useRef<HTMLInputElement>(null)
  const [name, setName] = useState(room.name)
  const [capacity, setCapacity] = useState(room.capacity)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  useEffect(() => {
    nameRef.current?.focus()
  }, [])

  useEffect(() => {
    const handleKey = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape' && !loading) onClose()
    }
    document.addEventListener('keydown', handleKey)
    return () => document.removeEventListener('keydown', handleKey)
  }, [loading, onClose])

  const handleFocusTrap = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'Tab') return
    const focusable = dialogRef.current?.querySelectorAll<HTMLElement>(
      'input, button, [tabindex]:not([tabindex="-1"])',
    )
    if (!focusable || focusable.length === 0) return
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault()
      first.focus()
    }
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const roomName = name.trim()
    if (!roomName) {
      setError('채팅방 이름을 입력해 주세요.')
      return
    }
    if (!Number.isInteger(capacity) || capacity < 1 || capacity > 1000) {
      setError('정원은 1명 이상 1000명 이하로 입력해 주세요.')
      return
    }
    setLoading(true)
    setError('')
    try {
      await updateRoom(room.id, roomName, capacity)
      toast.success('채팅방이 수정되었습니다.')
      onUpdated()
      onClose()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '수정에 실패했습니다.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div
      className="fixed inset-0 bg-black/40 flex items-center justify-center z-50 p-4"
      onClick={() => !loading && onClose()}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={error ? errorId : undefined}
        className="bg-white rounded-lg p-6 w-full max-w-sm shadow-xl"
        onClick={(e) => e.stopPropagation()}
        onKeyDown={handleFocusTrap}
      >
        <h2 id={titleId} className="text-lg font-bold text-gray-900 mb-4">채팅방 수정</h2>
        <form onSubmit={handleSubmit} className="space-y-3">
          <div>
            <label htmlFor={nameId} className="block text-sm font-medium text-gray-700 mb-1">채팅방 이름</label>
            <input
              id={nameId}
              ref={nameRef}
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              minLength={1}
              maxLength={50}
              disabled={loading}
              aria-invalid={Boolean(error)}
              aria-describedby={error ? errorId : undefined}
              className="w-full px-3 py-2.5 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500"
            />
          </div>
          <div>
            <label htmlFor={capacityId} className="block text-sm font-medium text-gray-700 mb-1">정원</label>
            <input
              id={capacityId}
              type="number"
              value={capacity}
              onChange={(e) => setCapacity(Number(e.target.value))}
              min={1}
              max={1000}
              step={1}
              inputMode="numeric"
              required
              disabled={loading}
              aria-invalid={Boolean(error)}
              aria-describedby={error ? errorId : undefined}
              className="w-full px-3 py-2.5 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500"
            />
          </div>
          {error && <p id={errorId} role="alert" className="text-red-500 text-sm">{error}</p>}
          <div className="flex gap-2 pt-1">
            <button
              type="button"
              onClick={onClose}
              disabled={loading}
              className="flex-1 py-2.5 border border-gray-300 rounded-lg text-sm text-gray-700 hover:bg-gray-50 transition-colors"
            >
              취소
            </button>
            <button
              type="submit"
              disabled={loading}
              className="flex-1 py-2.5 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 disabled:opacity-50 transition-colors"
            >
              {loading ? '저장 중...' : '저장'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

export default function MyRoomsPage() {
  const [rooms, setRooms] = useState<RoomInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [editingRoom, setEditingRoom] = useState<RoomInfo | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [confirmAction, setConfirmAction] = useState<{
    type: 'leave' | 'delete'
    room: RoomInfo
  } | null>(null)
  const [actionLoading, setActionLoading] = useState(false)
  const [error, setError] = useState('')
  const navigate = useNavigate()
  const { userId } = useAuth()
  const toast = useToast()

  const fetchRooms = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await listJoinedRooms()
      setRooms(data.rooms ?? [])
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : '내 채팅방 목록을 불러오지 못했습니다. 네트워크 상태를 확인한 뒤 다시 시도해 주세요.',
      )
      setRooms([])
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    fetchRooms()
  }, [fetchRooms])

  const handleConfirm = async () => {
    if (!confirmAction) return
    setActionLoading(true)
    try {
      if (confirmAction.type === 'leave') {
        await leaveRoom(confirmAction.room.id)
        toast.success(`"${confirmAction.room.name}" 채팅방에서 나갔습니다.`)
      } else {
        await deleteRoom(confirmAction.room.id)
        toast.success(`"${confirmAction.room.name}" 채팅방이 삭제되었습니다.`)
      }
      setConfirmAction(null)
      await fetchRooms()
    } catch (err) {
      toast.error(
        err instanceof ApiError
          ? err.message
          : '요청을 처리하지 못했습니다. 잠시 후 다시 시도해 주세요.',
      )
    } finally {
      setActionLoading(false)
    }
  }

  return (
    <div className="max-w-5xl mx-auto px-4 py-5 space-y-4">
      {error && rooms.length > 0 && <ErrorBanner message={error} onClose={() => setError('')} />}

      <div className="flex items-center justify-between">
        <p className="text-xs text-gray-400">참여 중인 채팅방 {rooms.length}개</p>
        <button
          onClick={() => setShowCreate(true)}
          className="px-3.5 py-1.5 bg-indigo-600 text-white rounded-lg text-xs font-medium hover:bg-indigo-700 transition-colors"
        >
          + 만들기
        </button>
      </div>

      {loading ? (
        <div className="py-16 text-center text-sm text-gray-400">불러오는 중...</div>
      ) : error && rooms.length === 0 ? (
        <EmptyState
          icon="chat"
          title="내 채팅방을 불러오지 못했습니다."
          description={error}
          action={{ label: '다시 시도', onClick: fetchRooms }}
        />
      ) : rooms.length === 0 ? (
        <EmptyState
          icon="chat"
          title="참여 중인 채팅방이 없습니다."
          description="로비에서 채팅방을 찾아 참여해 보세요."
          action={{ label: '로비로 이동', onClick: () => navigate('/lobby') }}
        />
      ) : (
        <div className="grid gap-2 lg:grid-cols-2">
          {rooms.map((room) => {
            const isManager = room.manager_id === userId
            return (
              <div
                key={room.id}
                className="bg-white rounded-lg border border-gray-100 hover:border-indigo-200 transition-colors cursor-pointer"
                onClick={() =>
                  navigate(`/chat/${room.id}`, { state: { roomName: room.name } })
                }
              >
                <div className="p-4 flex items-center justify-between">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <p className="font-medium text-gray-900 text-sm truncate">{room.name}</p>
                      {isManager && (
                        <span className="text-xs text-indigo-600 bg-indigo-50 px-2 py-0.5 rounded font-medium shrink-0">
                          방장
                        </span>
                      )}
                    </div>
                    <p className="text-xs text-gray-400 mt-0.5">
                      {room.member_count}/{room.capacity}명
                    </p>
                  </div>
                  <div
                    className="flex items-center gap-0.5 shrink-0"
                    onClick={(e) => e.stopPropagation()}
                  >
                    {isManager && (
                      <>
                        <button
                          onClick={() => setEditingRoom(room)}
                          className="px-2 py-1.5 text-xs text-gray-400 hover:text-indigo-600 rounded-lg hover:bg-indigo-50 transition-colors"
                        >
                          수정
                        </button>
                        <button
                          onClick={() => setConfirmAction({ type: 'delete', room })}
                          className="px-2 py-1.5 text-xs text-gray-400 hover:text-red-500 rounded-lg hover:bg-red-50 transition-colors"
                        >
                          삭제
                        </button>
                      </>
                    )}
                    <button
                      onClick={() => setConfirmAction({ type: 'leave', room })}
                      className="px-2 py-1.5 text-xs text-gray-400 hover:text-orange-500 rounded-lg hover:bg-orange-50 transition-colors"
                    >
                      나가기
                    </button>
                  </div>
                </div>
              </div>
            )
          })}
        </div>
      )}

      {confirmAction && (
        <ConfirmModal
          title={confirmAction.type === 'delete' ? '채팅방 삭제' : '채팅방 나가기'}
          description={
            confirmAction.type === 'delete'
              ? `"${confirmAction.room.name}" 채팅방을 삭제하시겠습니까? 이 작업은 되돌릴 수 없습니다.`
              : `"${confirmAction.room.name}" 채팅방에서 나가시겠습니까?`
          }
          confirmLabel={confirmAction.type === 'delete' ? '삭제' : '나가기'}
          danger
          loading={actionLoading}
          onConfirm={handleConfirm}
          onCancel={() => setConfirmAction(null)}
        />
      )}

      {editingRoom && (
        <EditRoomModal
          room={editingRoom}
          onClose={() => setEditingRoom(null)}
          onUpdated={fetchRooms}
        />
      )}

      {showCreate && (
        <CreateRoomModal
          onClose={() => setShowCreate(false)}
          onCreated={(roomId, roomName) => {
            toast.success('채팅방이 생성되었습니다.')
            navigate(`/chat/${roomId}`, { state: { roomName } })
          }}
        />
      )}
    </div>
  )
}
