import { useId, useRef } from 'react'
import { useDialog } from '../hooks/useDialog'

interface Props {
  memberIds: string[]
  userMap: ReadonlyMap<string, string>
  userId: string
  managerId: string | null
  onClose: () => void
}

export default function RoomMembers({ memberIds, userMap, userId, managerId, onClose }: Props) {
  const membersTitleId = useId()
  const closeRef = useRef<HTMLButtonElement>(null)
  const { dialogRef, handleFocusTrap } = useDialog(onClose, false, closeRef)

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div className="absolute inset-0 bg-black/30" />
      <div
        ref={dialogRef}
        tabIndex={-1}
        onKeyDown={handleFocusTrap}
        role="dialog"
        aria-modal="true"
        aria-labelledby={membersTitleId}
        className="relative w-72 max-w-[80vw] bg-white h-full shadow-xl flex flex-col animate-slide-in"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-4 py-4 border-b border-gray-200 flex items-center justify-between">
          <span id={membersTitleId} className="text-sm font-semibold text-gray-900">멤버 ({memberIds.length})</span>
          <button
            ref={closeRef}
            type="button"
            onClick={onClose}
            className="text-gray-400 hover:text-gray-600"
            aria-label="멤버 목록 닫기"
          >
            <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
        </div>
        <div className="flex-1 overflow-y-auto">
          {memberIds.map((id) => (
            <div key={id} className="px-4 py-3 flex items-center justify-between border-b border-gray-50">
              <div className="flex items-center gap-2 min-w-0">
                <span className="text-sm text-gray-900 truncate">{userMap.get(id) ?? '(알 수 없음)'}</span>
                {id === userId && (
                  <span className="text-xs text-indigo-600 bg-indigo-50 px-2 py-0.5 rounded font-medium shrink-0">나</span>
                )}
              </div>
              {id === managerId && (
                <span className="text-xs text-amber-600 bg-amber-50 px-2 py-0.5 rounded font-medium shrink-0">방장</span>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}
