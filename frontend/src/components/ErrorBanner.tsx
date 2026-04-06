interface ErrorBannerProps {
  message: string
  onClose: () => void
}

export default function ErrorBanner({ message, onClose }: ErrorBannerProps) {
  return (
    <div className="bg-red-50 text-red-600 px-4 py-2.5 rounded-lg text-sm flex items-center justify-between">
      <span>{message}</span>
      <button onClick={onClose} className="font-medium ml-3 shrink-0 hover:text-red-700">
        닫기
      </button>
    </div>
  )
}
