import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, listRoomMembers } from '../api/client'
import { createRefreshQueue } from '../refreshQueue'
import type { RoomMember } from '../types'

export function useRoomMembers(roomId: string, onMembers: (members: RoomMember[]) => void) {
  const [memberIds, setMemberIds] = useState<string[]>([])
  const [membersError, setMembersError] = useState('')
  const queue = useRef<ReturnType<typeof createRefreshQueue<RoomMember[]>> | null>(null)

  useEffect(() => {
    const active = createRefreshQueue({
      fetch: async (signal) => {
        const response = await listRoomMembers(roomId, AbortSignal.any([signal, AbortSignal.timeout(10000)]))
        return response.members ?? []
      },
      onSuccess: (members) => {
        setMemberIds(members.map((member) => member.user_id))
        onMembers(members)
        setMembersError('')
      },
      onError: () => setMembersError('멤버 목록을 갱신하지 못했습니다. 표시된 정보가 최신이 아닐 수 있습니다.'),
      retryDelay: (error, attempt) => {
        const backoff = 1000 * 2 ** attempt + Math.random() * 250
        if (error instanceof ApiError) {
          if (error.status === 429) return Math.max(backoff, (error.retryAfterSeconds ?? 0) * 1000)
          if (error.status < 500 || error.status >= 600) return null
        } else if (!(error instanceof TypeError) && !(error instanceof DOMException && error.name === 'TimeoutError')) {
          return null
        }
        return backoff
      },
    })
    queue.current = active
    return () => {
      active.dispose()
      queue.current = null
    }
  }, [roomId, onMembers])

  const refreshMembers = useCallback(() => queue.current?.refresh(), [])
  return { memberIds, membersError, refreshMembers }
}
