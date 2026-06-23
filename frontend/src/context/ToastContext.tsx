import { useState, useCallback, useEffect, useRef, type ReactNode } from 'react'
import { ToastContext } from './toast'

interface Toast {
  id: number
  message: string
  type: 'success' | 'error'
}

let nextId = 0

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])

  const add = useCallback((message: string, type: 'success' | 'error') => {
    const id = ++nextId
    setToasts((prev) => [...prev, { id, message, type }])
    setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id))
    }, 3000)
  }, [])

  const success = useCallback((message: string) => add(message, 'success'), [add])
  const error = useCallback((message: string) => add(message, 'error'), [add])

  return (
    <ToastContext.Provider value={{ success, error }}>
      {children}
      {/* Toast container */}
      <div
        aria-live="polite"
        aria-atomic="false"
        className="fixed bottom-6 left-1/2 -translate-x-1/2 z-[100] flex flex-col gap-2 items-center pointer-events-none"
      >
        {toasts.map((t) => (
          <ToastItem key={t.id} toast={t} onClose={() => setToasts((prev) => prev.filter((x) => x.id !== t.id))} />
        ))}
      </div>
    </ToastContext.Provider>
  )
}

function ToastItem({ toast, onClose }: { toast: Toast; onClose: () => void }) {
  const [visible, setVisible] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined)

  useEffect(() => {
    requestAnimationFrame(() => setVisible(true))
    timerRef.current = setTimeout(() => setVisible(false), 2600)
    return () => clearTimeout(timerRef.current)
  }, [])

  const handleTransitionEnd = () => {
    if (!visible) onClose()
  }

  return (
    <div
      role={toast.type === 'error' ? 'alert' : 'status'}
      aria-live={toast.type === 'error' ? 'assertive' : 'polite'}
      onTransitionEnd={handleTransitionEnd}
      className={`pointer-events-auto flex items-center gap-3 px-4 py-2.5 rounded-lg shadow-lg text-sm font-medium transition-all duration-300 ${
        visible ? 'opacity-100 translate-y-0' : 'opacity-0 translate-y-2'
      } ${
        toast.type === 'success'
          ? 'bg-gray-900 text-white'
          : 'bg-red-600 text-white'
      }`}
    >
      <span>{toast.message}</span>
      <button
        type="button"
        onClick={() => setVisible(false)}
        className="text-current/80 hover:text-current"
        aria-label="알림 닫기"
      >
        <svg aria-hidden="true" className="size-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18 18 6M6 6l12 12" />
        </svg>
      </button>
    </div>
  )
}
