import { useEffect, useState } from 'react'

export default function ReconnectNotice() {
  const [notice, setNotice] = useState<'short' | 'long' | null>(null)

  useEffect(() => {
    const shortTimer = setTimeout(() => setNotice('short'), 300)
    const longTimer = setTimeout(() => setNotice('long'), 10000)
    return () => {
      clearTimeout(shortTimer)
      clearTimeout(longTimer)
    }
  }, [])

  if (!notice) return null
  return (
    <div className="bg-amber-50 border-b border-amber-100 text-amber-800 text-xs">
      <div className="max-w-4xl mx-auto px-4 py-2">
        {notice === 'long'
          ? '연결 복구가 지연되고 있습니다. 작성 중인 메시지는 연결 후 직접 보내 주세요.'
          : '채팅방 재접속 중입니다'}
      </div>
    </div>
  )
}
