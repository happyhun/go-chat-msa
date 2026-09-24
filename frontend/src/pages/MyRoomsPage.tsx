import { useState, useEffect, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import { listJoinedRooms, deleteRoom, leaveRoom, ApiError } from '../api/client'
import { useAuth } from '../context/auth'
import { useToast } from '../context/toast'
import type { RoomInfo } from '../types'
import ErrorBanner from '../components/ErrorBanner'
import EmptyState from '../components/EmptyState'
import CreateRoomModal from '../components/CreateRoomModal'
import EditRoomModal from '../components/EditRoomModal'
import ConfirmModal from '../components/ConfirmModal'

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
