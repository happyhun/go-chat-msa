import { useEffect, useEffectEvent, useRef, type KeyboardEvent, type RefObject } from 'react'

const FOCUSABLE = 'a[href], button, input, select, textarea, [tabindex]:not([tabindex="-1"])'

export function useDialog(onClose: () => void, loading: boolean, initialFocus: RefObject<HTMLElement | null>) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const onEscape = useEffectEvent((event: globalThis.KeyboardEvent) => {
    if (event.key === 'Escape' && !loading) onClose()
  })

  useEffect(() => {
    const previousFocus = document.activeElement
    initialFocus.current?.focus()
    document.addEventListener('keydown', onEscape)
    return () => {
      document.removeEventListener('keydown', onEscape)
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) previousFocus.focus()
    }
  }, [initialFocus])

  const handleFocusTrap = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== 'Tab') return
    const dialog = dialogRef.current
    if (!dialog) return
    const focusable = [...dialog.querySelectorAll<HTMLElement>(FOCUSABLE)]
      .filter((element) => !element.matches(':disabled, [hidden], [aria-hidden="true"]') && element.tabIndex >= 0)
    const first = focusable[0]
    const last = focusable.at(-1)
    if (!first || !last) {
      event.preventDefault()
      dialog.focus()
    } else if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }

  return { dialogRef, handleFocusTrap }
}
