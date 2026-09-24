import { updateRoom } from '../api/client'
import { useToast } from '../context/toast'
import type { RoomInfo } from '../types'
import RoomFormModal from './RoomFormModal'

interface Props {
  room: RoomInfo
  onClose: () => void
  onUpdated: () => void
}

export default function EditRoomModal({ room, onClose, onUpdated }: Props) {
  const toast = useToast()
  const update = async (name: string, capacity: number) => {
    await updateRoom(room.id, name, capacity)
    toast.success('채팅방이 수정되었습니다.')
    onUpdated()
  }

  return (
    <RoomFormModal
      key={room.id}
      title="채팅방 수정"
      initialName={room.name}
      initialCapacity={room.capacity}
      submitLabel="저장"
      pendingLabel="저장 중..."
      errorMessage="수정에 실패했습니다."
      onSubmit={update}
      onClose={onClose}
    />
  )
}
