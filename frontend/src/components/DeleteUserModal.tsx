import { useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'

interface Props {
  onConfirm: (password: string) => Promise<void>
  onCancel: () => void
}

export default function DeleteUserModal({ onConfirm, onCancel }: Props) {
  const titleId = useId()
  const errorId = useId()
  const passwordRef = useRef<HTMLInputElement>(null)
  const dialogRef = useRef<HTMLDivElement>(null)

  const [password, setPassword] = useState('')
  const [agreed, setAgreed] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    passwordRef.current?.focus()
  }, [])

  useEffect(() => {
    const handleKeyDown = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape' && !loading) onCancel()
    }
    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [loading, onCancel])

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault()
    if (loading || !password || !agreed) return
    setLoading(true)
    setError(null)
    try {
      await onConfirm(password)
    } catch (err) {
      setError(err instanceof Error ? err.message : '오류가 발생했습니다. 잠시 후 다시 시도해 주세요.')
      setPassword('')
      passwordRef.current?.focus()
      setLoading(false)
    }
  }

  const handleFocusTrap = (e: KeyboardEvent<HTMLDivElement>) => {
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

  const canSubmit = password.length > 0 && agreed && !loading

  return (
    <div
      className="fixed inset-0 bg-black/40 flex items-center justify-center z-50 p-4"
      onClick={() => !loading && onCancel()}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="bg-white rounded-2xl p-6 w-full max-w-sm shadow-xl"
        onClick={(e) => e.stopPropagation()}
        onKeyDown={handleFocusTrap}
      >
        <h3 id={titleId} className="text-base font-bold text-gray-900">
          정말 탈퇴하시겠어요?
        </h3>
        <ul className="mt-3 text-sm text-gray-600 space-y-1.5 list-disc pl-5">
          <li>30일 동안 동일 사용자 이름으로 재가입할 수 없습니다.</li>
          <li>회원님이 만든 채팅방의 권한은 가장 오래된 멤버에게 위임됩니다.</li>
        </ul>

        <form onSubmit={handleSubmit} className="mt-5">
          <label className="block text-sm font-medium text-gray-700 mb-1.5">
            비밀번호
            <input
              ref={passwordRef}
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => {
                setPassword(e.target.value)
                if (error) setError(null)
              }}
              disabled={loading}
              className="mt-1 w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-red-500 disabled:bg-gray-100"
            />
          </label>

          {error && (
            <p id={errorId} role="alert" aria-live="polite" className="mt-2 text-sm text-red-600">
              {error}
            </p>
          )}

          <label className="mt-4 flex items-start gap-2 text-sm text-gray-700 cursor-pointer">
            <input
              type="checkbox"
              checked={agreed}
              onChange={(e) => setAgreed(e.target.checked)}
              disabled={loading}
              className="mt-0.5 size-4 rounded border-gray-300 text-red-600 focus:ring-red-500"
            />
            <span>위 내용을 확인했습니다</span>
          </label>

          <div className="flex gap-2 mt-5">
            <button
              type="button"
              onClick={onCancel}
              disabled={loading}
              className="flex-1 py-2.5 border border-gray-300 rounded-lg text-sm text-gray-700 hover:bg-gray-50 transition-colors disabled:opacity-50"
            >
              취소
            </button>
            <button
              type="submit"
              disabled={!canSubmit}
              aria-describedby={error ? errorId : undefined}
              className="flex-1 py-2.5 rounded-lg text-sm font-medium bg-red-600 text-white hover:bg-red-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {loading ? '처리 중...' : '탈퇴'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
