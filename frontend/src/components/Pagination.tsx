interface PaginationProps {
  page: number
  totalPages: number
  disabled?: boolean
  onChange: (page: number) => void
}

export default function Pagination({ page, totalPages, disabled, onChange }: PaginationProps) {
  if (totalPages <= 1) return null

  const pages = Array.from({ length: totalPages }, (_, i) => i)
    .filter((i) => i === 0 || i === totalPages - 1 || Math.abs(i - page) <= 2)
    .reduce<(number | 'ellipsis')[]>((acc, i, idx, arr) => {
      if (idx > 0 && arr[idx - 1] !== i - 1) acc.push('ellipsis')
      acc.push(i)
      return acc
    }, [])

  return (
    <div className="flex items-center justify-center gap-1 pt-3 pb-1">
      <button
        onClick={() => onChange(page - 1)}
        disabled={page === 0 || disabled}
        className="px-2.5 py-1.5 text-xs text-gray-600 rounded-lg hover:bg-gray-100 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
      >
        이전
      </button>
      {pages.map((item, idx) =>
        item === 'ellipsis' ? (
          <span key={`e-${idx}`} className="px-1.5 text-xs text-gray-400">
            ...
          </span>
        ) : (
          <button
            key={item}
            onClick={() => onChange(item)}
            disabled={disabled}
            className={`w-8 h-8 text-xs rounded-lg transition-colors ${
              item === page
                ? 'bg-indigo-600 text-white font-medium'
                : 'text-gray-600 hover:bg-gray-100'
            } disabled:opacity-50`}
          >
            {item + 1}
          </button>
        ),
      )}
      <button
        onClick={() => onChange(page + 1)}
        disabled={page >= totalPages - 1 || disabled}
        className="px-2.5 py-1.5 text-xs text-gray-600 rounded-lg hover:bg-gray-100 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
      >
        다음
      </button>
    </div>
  )
}
