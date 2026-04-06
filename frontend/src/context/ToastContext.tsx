import { createContext, useContext, useState, useCallback, useEffect, useRef, type ReactNode } from 'react'

interface Toast {
  id: number
  message: string
  type: 'success' | 'error'
}

interface ToastContextValue {
  success: (message: string) => void
  error: (message: string) => void
}

const ToastContext = createContext<ToastContextValue>(null!)

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
      <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-[100] flex flex-col gap-2 items-center pointer-events-none">
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
      onTransitionEnd={handleTransitionEnd}
      className={`pointer-events-auto px-4 py-2.5 rounded-xl shadow-lg text-sm font-medium transition-all duration-300 ${
        visible ? 'opacity-100 translate-y-0' : 'opacity-0 translate-y-2'
      } ${
        toast.type === 'success'
          ? 'bg-gray-900 text-white'
          : 'bg-red-600 text-white'
      }`}
    >
      {toast.message}
    </div>
  )
}

export function useToast() {
  return useContext(ToastContext)
}
