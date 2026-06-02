import { createContext, useContext } from 'react'

export interface ToastContextValue {
  success: (message: string) => void
  error: (message: string) => void
}

export const ToastContext = createContext<ToastContextValue>(null!)

export function useToast() {
  return useContext(ToastContext)
}
