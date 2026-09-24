import { useState, useId, useRef, type FormEvent } from 'react'
import { ApiError } from '../api/client'
import { useDialog } from '../hooks/useDialog'

interface Props {
  title: string
  initialName?: string
  initialCapacity?: number
  submitLabel: string
  pendingLabel: string
  errorMessage: string
  onSubmit: (name: string, capacity: number) => Promise<void>
  onClose: () => void
}

export default function RoomFormModal({
  title, initialName = '', initialCapacity = 100,
  submitLabel, pendingLabel, errorMessage, onSubmit, onClose,
}: Props) {
  const titleId = useId()
  const errorId = useId()
  const nameId = useId()
  const capacityId = useId()
  const nameRef = useRef<HTMLInputElement>(null)
  const [name, setName] = useState(initialName)
  const [capacity, setCapacity] = useState(initialCapacity)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const { dialogRef, handleFocusTrap } = useDialog(onClose, loading, nameRef)

  const handleSubmit = async (e: FormEvent) => {
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
      await onSubmit(roomName, capacity)
      onClose()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : errorMessage)
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
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={error ? errorId : undefined}
        className="bg-white rounded-lg p-6 w-full max-w-sm shadow-xl"
        onClick={(e) => e.stopPropagation()}
        onKeyDown={handleFocusTrap}
      >
        <h2 id={titleId} className="text-lg font-bold text-gray-900 mb-4">{title}</h2>
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
              {loading ? pendingLabel : submitLabel}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
