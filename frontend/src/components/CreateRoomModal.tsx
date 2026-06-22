import { useState, useEffect, useId, useRef, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { createRoom, ApiError } from '../api/client'

interface Props {
  onClose: () => void
  onCreated: (roomId: string, roomName: string) => void
}

export default function CreateRoomModal({ onClose, onCreated }: Props) {
  const titleId = useId()
  const errorId = useId()
  const nameId = useId()
  const capacityId = useId()
  const dialogRef = useRef<HTMLDivElement>(null)
  const nameRef = useRef<HTMLInputElement>(null)
  const [name, setName] = useState('')
  const [capacity, setCapacity] = useState(100)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

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
      const res = await createRoom(roomName, capacity)
      onCreated(res.room_id, roomName)
      onClose()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '채팅방 생성에 실패했습니다.')
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
        <h2 id={titleId} className="text-lg font-bold text-gray-900 mb-4">새 채팅방</h2>
        <form onSubmit={handleSubmit} className="space-y-3">
          <div>
            <label htmlFor={nameId} className="block text-sm font-medium text-gray-700 mb-1">채팅방 이름</label>
            <input
              id={nameId}
              ref={nameRef}
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="채팅방 이름을 입력하세요"
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
              {loading ? '생성 중...' : '만들기'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
