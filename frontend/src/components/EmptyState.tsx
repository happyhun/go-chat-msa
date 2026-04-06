interface EmptyStateProps {
  icon: 'chat' | 'search' | 'room'
  title: string
  description?: string
  action?: { label: string; onClick: () => void }
}

const icons = {
  chat: (
    <svg className="w-10 h-10 text-gray-300" fill="none" viewBox="0 0 24 24" stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
    </svg>
  ),
  search: (
    <svg className="w-10 h-10 text-gray-300" fill="none" viewBox="0 0 24 24" stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
    </svg>
  ),
  room: (
    <svg className="w-10 h-10 text-gray-300" fill="none" viewBox="0 0 24 24" stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0z" />
    </svg>
  ),
}

export default function EmptyState({ icon, title, description, action }: EmptyStateProps) {
  return (
    <div className="py-16 flex flex-col items-center text-center">
      {icons[icon]}
      <p className="mt-3 text-sm font-medium text-gray-500">{title}</p>
      {description && <p className="mt-1 text-xs text-gray-400">{description}</p>}
      {action && (
        <button
          onClick={action.onClick}
          className="mt-4 text-sm text-indigo-600 hover:text-indigo-700 font-medium"
        >
          {action.label}
        </button>
      )}
    </div>
  )
}
