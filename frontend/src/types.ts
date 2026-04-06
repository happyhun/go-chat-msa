export interface ProblemDetails {
  type: string
  title: string
  status: number
  detail: string
}

export interface RoomInfo {
  id: string
  name: string
  manager_id: string
  capacity: number
  member_count: number
  joined_at?: string
}

export interface RoomMember {
  user_id: string
  username: string
  joined_at: string
}

export interface MessageInfo {
  id: string
  room_id: string
  sender_id: string
  content: string
  type: 'chat' | 'system' | 'conflict'
  timestamp: number
  sequence_number: number
}

export interface WsOutgoing {
  id: string
  room_id: string
  sender_id: string
  content: string
  type: 'chat' | 'system' | 'conflict'
  client_msg_id?: string
  sequence_number: number
  timestamp: number
}
