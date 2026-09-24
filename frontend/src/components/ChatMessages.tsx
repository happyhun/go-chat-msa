import type { MessageInfo } from '../types'

interface Props {
  messages: MessageInfo[]
  userId: string
  userMap: ReadonlyMap<string, string>
  retryMessage: (message: MessageInfo) => void
}

const timeFormatter = new Intl.DateTimeFormat('ko-KR', { hour: '2-digit', minute: '2-digit' })

function formatTime(unix: number) {
  return timeFormatter.format(new Date(unix * 1000))
}

export default function ChatMessages({ messages, userId, userMap, retryMessage }: Props) {
  return (
    <>
      {messages.map((msg, i) => {
        if (msg.type === 'system') {
          return (
            <div key={msg.id} className="text-center py-2">
              <span className="text-xs text-gray-400 bg-gray-100 px-3 py-1 rounded-full">
                {msg.content}
              </span>
            </div>
          )
        }

        const isMine = msg.sender_id === userId
        const prev = messages[i - 1]
        const next = messages[i + 1]
        const sameSenderAsPrev =
          prev && prev.type !== 'system' && prev.sender_id === msg.sender_id
        const sameSenderAsNext =
          next && next.type !== 'system' && next.sender_id === msg.sender_id
        const showName = !isMine && !sameSenderAsPrev
        const showTime = !sameSenderAsNext
        const senderName = userMap.get(msg.sender_id) ?? '(탈퇴한 사용자)'

        return (
          <div
            key={msg.id}
            className={`flex ${isMine ? 'justify-end' : 'justify-start'} ${sameSenderAsPrev ? 'mt-0.5' : 'mt-3'}`}
          >
            <div
              className={`flex flex-col ${isMine ? 'items-end' : 'items-start'} max-w-[70%]`}
            >
              {showName && senderName && (
                <span className="text-xs text-gray-500 mb-0.5 px-1">{senderName}</span>
              )}
              <div
                className={`px-3.5 py-2 text-sm break-words whitespace-pre-wrap ${
                  isMine
                    ? `bg-indigo-600 text-white ${sameSenderAsPrev ? 'rounded-2xl rounded-tr-md' : 'rounded-2xl'} ${sameSenderAsNext ? 'rounded-br-md' : ''}`
                    : `bg-white text-gray-900 border border-gray-100 ${sameSenderAsPrev ? 'rounded-2xl rounded-tl-md' : 'rounded-2xl'} ${sameSenderAsNext ? 'rounded-bl-md' : ''}`
                }`}
              >
                {msg.content}
              </div>
              {(showTime || msg.status) && (
                <span className="text-xs text-gray-400 mt-0.5 px-1">
                  {formatTime(msg.timestamp)}
                  {msg.status && ` · ${msg.status === 'sending' ? '전송 중' : msg.status === 'accepted' ? '접수' : '결과 확인 필요'}`}
                </span>
              )}
              {isMine && msg.status === 'failed' && (
                <button className="text-xs text-indigo-600 underline mt-1" onClick={() => retryMessage(msg)}>
                  같은 메시지 다시 보내기
                </button>
              )}
            </div>
          </div>
        )
      })}
    </>
  )
}
