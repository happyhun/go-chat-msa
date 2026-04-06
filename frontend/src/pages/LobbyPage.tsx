import { useState, useEffect, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import { searchRooms, joinRoom, listJoinedRooms, ApiError } from '../api/client'
import { useToast } from '../context/ToastContext'
import type { RoomInfo } from '../types'
import ErrorBanner from '../components/ErrorBanner'
import EmptyState from '../components/EmptyState'
import Pagination from '../components/Pagination'
import CreateRoomModal from '../components/CreateRoomModal'

const PAGE_SIZE = 20

export default function LobbyPage() {
  const [rooms, setRooms] = useState<RoomInfo[]>([])
  const [totalCount, setTotalCount] = useState(0)
  const [page, setPage] = useState(0)
  const [query, setQuery] = useState('')
  const [activeQuery, setActiveQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [joinedIds, setJoinedIds] = useState<Set<string>>(new Set())
  const [showCreate, setShowCreate] = useState(false)
  const [error, setError] = useState('')
  const navigate = useNavigate()
  const toast = useToast()

  const fetchRooms = useCallback(async (q: string, p: number) => {
    setLoading(true)
    try {
      const data = await searchRooms(q, PAGE_SIZE, p * PAGE_SIZE)
      setRooms(data.rooms ?? [])
      setTotalCount(data.total_count)
    } catch (err) {
      if (err instanceof ApiError) setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  const fetchJoined = useCallback(async () => {
    try {
      const data = await listJoinedRooms()
      setJoinedIds(new Set((data.rooms ?? []).map((r) => r.id)))
    } catch {
      // ignore
    }
  }, [])

  useEffect(() => {
    fetchRooms('', 0)
    fetchJoined()
  }, [fetchRooms, fetchJoined])

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault()
    setActiveQuery(query)
    setPage(0)
    fetchRooms(query, 0)
  }

  const handleClearSearch = () => {
    setQuery('')
    setActiveQuery('')
    setPage(0)
    fetchRooms('', 0)
  }

  const handlePageChange = (p: number) => {
    setPage(p)
    fetchRooms(activeQuery, p)
  }

  const handleJoin = async (room: RoomInfo) => {
    try {
      await joinRoom(room.id)
      toast.success(`"${room.name}" 채팅방에 참여했습니다.`)
      navigate(`/chat/${room.id}`, { state: { roomName: room.name } })
    } catch (err) {
      if (err instanceof ApiError) toast.error(err.message)
    }
  }

  const totalPages = Math.ceil(totalCount / PAGE_SIZE)

  return (
    <div className="max-w-2xl mx-auto px-4 py-5 space-y-4">
      {error && <ErrorBanner message={error} onClose={() => setError('')} />}

      {/* Search + Create */}
      <div className="flex gap-2">
        <form onSubmit={handleSearch} className="flex-1 flex gap-2">
          <div className="relative flex-1">
            <input
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="채팅방 이름으로 검색..."
              className="w-full px-3 py-2.5 pl-9 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500"
            />
            <svg
              className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
            >
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
            </svg>
          </div>
          <button
            type="submit"
            disabled={loading}
            className="px-4 py-2.5 bg-gray-100 text-gray-700 rounded-lg text-sm hover:bg-gray-200 transition-colors shrink-0 disabled:opacity-50"
          >
            검색
          </button>
          {activeQuery && (
            <button
              type="button"
              onClick={handleClearSearch}
              className="px-3 py-2.5 text-sm text-gray-500 hover:text-gray-700 shrink-0"
            >
              초기화
            </button>
          )}
        </form>
        <button
          onClick={() => setShowCreate(true)}
          className="px-4 py-2.5 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 transition-colors shrink-0"
        >
          + 만들기
        </button>
      </div>

      {/* Status */}
      <p className="text-xs text-gray-400">
        {activeQuery ? (
          <>&ldquo;{activeQuery}&rdquo; 검색 결과 {totalCount}건</>
        ) : (
          <>전체 채팅방 {totalCount}개</>
        )}
      </p>

      {/* Room List */}
      {loading && rooms.length === 0 ? (
        <div className="py-16 text-center text-sm text-gray-400">불러오는 중...</div>
      ) : rooms.length === 0 ? (
        activeQuery ? (
          <EmptyState icon="search" title="검색 결과가 없습니다." description="다른 키워드로 검색해 보세요." />
        ) : (
          <EmptyState
            icon="room"
            title="아직 채팅방이 없습니다."
            description="첫 번째 채팅방을 만들어 보세요."
            action={{ label: '채팅방 만들기', onClick: () => setShowCreate(true) }}
          />
        )
      ) : (
        <div className="space-y-2">
          {rooms.map((room) => {
            const joined = joinedIds.has(room.id)
            return (
              <div
                key={room.id}
                className={`bg-white rounded-xl p-4 border flex items-center justify-between ${
                  joined
                    ? 'border-indigo-100 cursor-pointer hover:border-indigo-200'
                    : 'border-gray-100'
                } transition-colors`}
                onClick={
                  joined
                    ? () => navigate(`/chat/${room.id}`, { state: { roomName: room.name } })
                    : undefined
                }
              >
                <div className="min-w-0">
                  <p className="font-medium text-gray-900 text-sm truncate">{room.name}</p>
                  <p className="text-xs text-gray-400 mt-0.5">
                    {room.member_count}/{room.capacity}명
                  </p>
                </div>
                <div onClick={(e) => e.stopPropagation()}>
                  {joined ? (
                    <span className="text-xs text-green-600 font-medium bg-green-50 px-2.5 py-1 rounded-full">
                      참여 중
                    </span>
                  ) : (
                    <button
                      onClick={() => handleJoin(room)}
                      className="px-3.5 py-1.5 bg-indigo-600 text-white rounded-lg text-xs font-medium hover:bg-indigo-700 transition-colors"
                    >
                      참여
                    </button>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}

      <Pagination page={page} totalPages={totalPages} disabled={loading} onChange={handlePageChange} />

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
