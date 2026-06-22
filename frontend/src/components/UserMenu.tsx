import { useEffect, useId, useRef, useState } from 'react'

interface Props {
  username: string
  onLogout: () => void
  onDeleteUser: () => void
}

export default function UserMenu({ username, onLogout, onDeleteUser }: Props) {
  const menuId = useId()
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuItemsRef = useRef<Array<HTMLButtonElement | null>>([])

  useEffect(() => {
    if (!open) return
    requestAnimationFrame(() => menuItemsRef.current[0]?.focus())
    const handleClick = (e: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false)
        triggerRef.current?.focus()
      }
    }
    document.addEventListener('mousedown', handleClick)
    document.addEventListener('keydown', handleKey)
    return () => {
      document.removeEventListener('mousedown', handleClick)
      document.removeEventListener('keydown', handleKey)
    }
  }, [open])

  const close = () => setOpen(false)

  const focusMenuItem = (index: number) => {
    const items = menuItemsRef.current.filter(Boolean)
    if (items.length === 0) return
    const nextIndex = (index + items.length) % items.length
    items[nextIndex]?.focus()
  }

  const handleMenuKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    const items = menuItemsRef.current.filter(Boolean)
    const currentIndex = items.findIndex((item) => item === document.activeElement)

    if (e.key === 'ArrowDown') {
      e.preventDefault()
      focusMenuItem(currentIndex + 1)
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      focusMenuItem(currentIndex - 1)
    } else if (e.key === 'Home') {
      e.preventDefault()
      focusMenuItem(0)
    } else if (e.key === 'End') {
      e.preventDefault()
      focusMenuItem(items.length - 1)
    }
  }

  return (
    <div ref={containerRef} className="relative">
      <button
        ref={triggerRef}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown') {
            e.preventDefault()
            setOpen(true)
          }
        }}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        className="flex items-center gap-1 text-sm text-gray-700 hover:text-gray-900 transition-colors"
      >
        <span>{username}</span>
        <svg
          aria-hidden="true"
          width="14"
          height="14"
          viewBox="0 0 20 20"
          fill="currentColor"
          className={`transition-transform ${open ? 'rotate-180' : ''}`}
        >
          <path
            fillRule="evenodd"
            d="M5.23 7.21a.75.75 0 011.06.02L10 11.06l3.71-3.83a.75.75 0 111.08 1.04l-4.25 4.39a.75.75 0 01-1.08 0L5.21 8.27a.75.75 0 01.02-1.06z"
            clipRule="evenodd"
          />
        </svg>
      </button>

      {open && (
        <div
          id={menuId}
          role="menu"
          aria-label="사용자 메뉴"
          onKeyDown={handleMenuKeyDown}
          className="absolute right-0 mt-2 w-40 bg-white border border-gray-200 rounded-lg shadow-lg py-1 z-40"
        >
          <button
            ref={(el) => {
              menuItemsRef.current[0] = el
            }}
            type="button"
            role="menuitem"
            onClick={() => {
              close()
              onLogout()
            }}
            className="w-full text-left px-3 py-2 text-sm text-gray-700 hover:bg-gray-50 transition-colors"
          >
            로그아웃
          </button>
          <button
            ref={(el) => {
              menuItemsRef.current[1] = el
            }}
            type="button"
            role="menuitem"
            onClick={() => {
              close()
              onDeleteUser()
            }}
            className="w-full text-left px-3 py-2 text-sm text-red-600 hover:bg-red-50 transition-colors"
          >
            회원탈퇴
          </button>
        </div>
      )}
    </div>
  )
}
