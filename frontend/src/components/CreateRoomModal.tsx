import { createRoom } from '../api/client'
import RoomFormModal from './RoomFormModal'

interface Props {
  onClose: () => void
  onCreated: (roomId: string, roomName: string) => void
}

export default function CreateRoomModal({ onClose, onCreated }: Props) {
  const create = async (name: string, capacity: number) => {
    const { room_id } = await createRoom(name, capacity)
    onCreated(room_id, name)
  }

  return (
    <RoomFormModal
      title="새 채팅방"
      submitLabel="만들기"
      pendingLabel="생성 중..."
      errorMessage="채팅방 생성에 실패했습니다."
      onSubmit={create}
      onClose={onClose}
    />
  )
}
