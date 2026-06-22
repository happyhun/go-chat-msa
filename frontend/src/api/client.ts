import type { ProblemDetails } from '../types'

let accessToken: string | null = null
let currentUserId: string | null = null
let currentUsername: string | null = null
let refreshPromise: Promise<AuthSession | null> | null = null

export interface AuthSession {
  accessToken: string
  userId: string
  username: string
}

interface AccessTokenPayload {
  sub?: string
  username?: string
  exp?: number
}

export function getAccessToken() {
  return accessToken
}

export function setAuth(token: string | null, userId: string | null, username: string | null) {
  accessToken = token
  currentUserId = userId
  currentUsername = username
  if (token && userId && username) {
    sessionStorage.setItem('access_token', token)
    sessionStorage.setItem('user_id', userId)
    sessionStorage.setItem('username', username)
  } else {
    sessionStorage.removeItem('access_token')
    sessionStorage.removeItem('user_id')
    sessionStorage.removeItem('username')
  }
}

export function restoreAuth() {
  accessToken = sessionStorage.getItem('access_token')
  currentUserId = sessionStorage.getItem('user_id')
  currentUsername = sessionStorage.getItem('username')
  if (accessToken && (!currentUserId || !currentUsername)) {
    const session = sessionFromAccessToken(accessToken)
    if (session) setAuth(session.accessToken, session.userId, session.username)
  }
}

export function getCurrentUserId() {
  return currentUserId
}

export function getCurrentUsername() {
  return currentUsername
}

function decodeBase64Url(value: string) {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/')
  const padded = normalized.padEnd(normalized.length + (4 - (normalized.length % 4)) % 4, '=')
  const binary = atob(padded)
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0))
  return new TextDecoder().decode(bytes)
}

function parseAccessToken(token: string): AccessTokenPayload | null {
  const payload = token.split('.')[1]
  if (!payload) return null
  try {
    return JSON.parse(decodeBase64Url(payload)) as AccessTokenPayload
  } catch {
    return null
  }
}

export function isAccessTokenExpired(token: string, skewSeconds = 30) {
  const payload = parseAccessToken(token)
  if (!payload?.exp) return true
  return payload.exp <= Math.floor(Date.now() / 1000) + skewSeconds
}

function sessionFromAccessToken(token: string): AuthSession | null {
  const payload = parseAccessToken(token)
  if (!payload?.sub || !payload.username) return null
  return {
    accessToken: token,
    userId: payload.sub,
    username: payload.username,
  }
}

function setAuthFromAccessToken(token: string): AuthSession | null {
  const session = sessionFromAccessToken(token)
  if (!session) return null
  setAuth(session.accessToken, session.userId, session.username)
  return session
}

const errorMessages: Record<string, string> = {
  // Auth
  'invalid request body': '요청 형식이 올바르지 않습니다.',
  'username and password are required': '사용자 이름과 비밀번호를 입력해 주세요.',
  'invalid username or password': '사용자 이름 또는 비밀번호가 올바르지 않습니다.',
  'username already exists': '이미 사용 중인 이름입니다.',
  'missing refresh token': '로그인이 만료되었습니다. 다시 로그인해 주세요.',
  'refresh token reuse detected': '보안 문제가 감지되었습니다. 다시 로그인해 주세요.',
  'refresh token expired': '로그인이 만료되었습니다. 다시 로그인해 주세요.',
  'invalid refresh token': '로그인이 만료되었습니다. 다시 로그인해 주세요.',
  'unauthorized': '로그인이 필요합니다.',
  'missing token': '로그인이 필요합니다.',
  'invalid token': '인증 정보가 유효하지 않습니다. 다시 로그인해 주세요.',
  'password is required': '비밀번호를 입력해 주세요.',
  'invalid password': '비밀번호가 올바르지 않습니다.',
  'user not found': '사용자를 찾을 수 없습니다.',

  // Room
  'room name is required': '채팅방 이름을 입력해 주세요.',
  'room not found': '채팅방을 찾을 수 없습니다.',
  'room is full': '채팅방 정원이 초과되었습니다.',
  'already a member of the room': '이미 참여 중인 채팅방입니다.',
  'not a member of the room': '참여 중인 채팅방이 아닙니다.',
  'only manager can update room': '방장만 채팅방을 수정할 수 있습니다.',
  'only manager can delete room': '방장만 채팅방을 삭제할 수 있습니다.',
  'capacity cannot be less than current member count': '정원은 현재 참여 인원보다 적을 수 없습니다.',
  'q parameter is required': '검색어를 입력해 주세요.',
  'limit exceeds maximum allowed': '요청한 개수가 허용 범위를 초과합니다.',

  // System
  'internal server error': '서버에 문제가 발생했습니다. 잠시 후 다시 시도해 주세요.',
}

const statusFallback: Record<number, string> = {
  400: '요청이 올바르지 않습니다.',
  401: '로그인이 필요합니다.',
  403: '권한이 없습니다.',
  404: '요청한 대상을 찾을 수 없습니다.',
  409: '요청을 처리할 수 없습니다.',
  429: '요청이 너무 많습니다. 잠시 후 다시 시도해 주세요.',
  500: '서버에 문제가 발생했습니다. 잠시 후 다시 시도해 주세요.',
  502: '서버에 연결할 수 없습니다.',
  503: '서비스가 일시적으로 중단되었습니다. 잠시 후 다시 시도해 주세요.',
  504: '서버 응답 시간이 초과되었습니다.',
}

function translateError(status: number, detail: string, retryAfterSeconds?: number | null): string {
  if (status === 429 && retryAfterSeconds && retryAfterSeconds > 0) {
    return `요청이 너무 많습니다. ${retryAfterSeconds}초 후 다시 시도해 주세요.`
  }

  const key = detail.toLowerCase().trim()
  for (const [pattern, msg] of Object.entries(errorMessages)) {
    if (key === pattern || key.includes(pattern)) return msg
  }
  return statusFallback[status] ?? '알 수 없는 오류가 발생했습니다.'
}

function parseRetryAfter(value: string | null): number | null {
  if (!value) return null

  const seconds = Number(value)
  if (Number.isFinite(seconds) && seconds > 0) {
    return Math.ceil(seconds)
  }

  const retryAt = Date.parse(value)
  if (Number.isNaN(retryAt)) return null

  const deltaSeconds = Math.ceil((retryAt - Date.now()) / 1000)
  return deltaSeconds > 0 ? deltaSeconds : null
}

export class ApiError extends Error {
  status: number
  problem: ProblemDetails
  retryAfterSeconds: number | null

  constructor(status: number, problem: ProblemDetails, retryAfterSeconds: number | null = null) {
    const detail = problem.detail || problem.title || ''
    super(translateError(status, detail, retryAfterSeconds))
    this.status = status
    this.problem = problem
    this.retryAfterSeconds = retryAfterSeconds
  }
}

async function tryRefresh(): Promise<AuthSession | null> {
  // 동시 요청 시 하나의 refresh만 실행
  if (refreshPromise) return refreshPromise

  refreshPromise = (async () => {
    try {
      const res = await fetch('/api/auth/token/refresh', {
        method: 'POST',
        credentials: 'include',
      })
      if (!res.ok) return null
      const data = await res.json()
      if (!data.access_token) return null
      return setAuthFromAccessToken(data.access_token)
    } catch {
      return null
    } finally {
      refreshPromise = null
    }
  })()

  return refreshPromise
}

export function refreshAuthSession() {
  return tryRefresh()
}

async function parseErrorResponse(res: Response): Promise<ApiError> {
  const retryAfterSeconds = parseRetryAfter(res.headers.get('Retry-After'))
  try {
    const problem = await res.json()
    return new ApiError(res.status, problem, retryAfterSeconds)
  } catch {
    return new ApiError(res.status, {
      type: 'about:blank',
      title: res.statusText || 'Error',
      status: res.status,
      detail: statusFallback[res.status] ?? '서버와 통신 중 오류가 발생했습니다.',
    }, retryAfterSeconds)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    ...(init?.headers as Record<string, string>),
  }

  if (accessToken) {
    headers['Authorization'] = `Bearer ${accessToken}`
  }

  if (init?.body && typeof init.body === 'string') {
    headers['Content-Type'] = 'application/json'
  }

  const res = await fetch(path, {
    ...init,
    headers,
    credentials: 'include',
  })

  if (res.status === 401 && accessToken) {
    const refreshed = await tryRefresh()
    if (refreshed) {
      headers['Authorization'] = `Bearer ${accessToken}`
      const retry = await fetch(path, { ...init, headers, credentials: 'include' })
      if (retry.status === 204) return undefined as T
      if (!retry.ok) throw await parseErrorResponse(retry)
      return retry.json()
    }
    setAuth(null, null, null)
    window.location.href = '/login'
    throw new Error('로그인이 만료되었습니다.')
  }

  if (res.status === 204) return undefined as T

  if (!res.ok) throw await parseErrorResponse(res)

  return res.json()
}

// Auth
export function signup(username: string, password: string) {
  return request<{ user_id: string }>('/api/users', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
}

export function login(username: string, password: string) {
  return request<{ user_id: string; access_token: string }>('/api/auth/token', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
}

export function logout() {
  return request<void>('/api/auth/token', { method: 'DELETE' })
}

export function deleteUser(password: string) {
  return request<void>('/api/me', {
    method: 'DELETE',
    body: JSON.stringify({ password }),
  })
}

export interface UserSummary {
  user_id: string
  username: string
}

export async function batchGetUsers(ids: string[]): Promise<UserSummary[]> {
  if (ids.length === 0) return []
  const chunks = chunk(ids, 100)
  const results = await Promise.all(
    chunks.map((c) => {
      const params = new URLSearchParams()
      for (const id of c) params.append('ids', id)
      return request<{ users: UserSummary[] }>(`/api/users?${params}`)
    }),
  )
  return results.flatMap((r) => r.users)
}

function chunk<T>(arr: T[], size: number): T[][] {
  const out: T[][] = []
  for (let i = 0; i < arr.length; i += size) out.push(arr.slice(i, i + size))
  return out
}

// Rooms
export function listJoinedRooms() {
  return request<{ rooms: import('../types').RoomInfo[] }>('/api/me/rooms')
}

export function searchRooms(q = '', limit = 20, offset = 0) {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) })
  if (q) params.set('q', q)
  return request<{ rooms: import('../types').RoomInfo[]; total_count: number }>(
    `/api/rooms?${params}`,
  )
}

export function createRoom(name: string, capacity: number) {
  return request<{ room_id: string }>('/api/rooms', {
    method: 'POST',
    body: JSON.stringify({ name, capacity }),
  })
}

export function updateRoom(id: string, name: string, capacity: number) {
  return request<void>(`/api/rooms/${id}`, {
    method: 'PATCH',
    body: JSON.stringify({ name, capacity }),
  })
}

export function deleteRoom(id: string) {
  return request<void>(`/api/rooms/${id}`, { method: 'DELETE' })
}

export function joinRoom(id: string) {
  return request<void>(`/api/rooms/${id}/members/me`, { method: 'PUT' })
}

export function leaveRoom(id: string) {
  return request<void>(`/api/rooms/${id}/members/me`, { method: 'DELETE' })
}

export function listRoomMembers(roomId: string) {
  return request<{ members: import('../types').RoomMember[] }>(`/api/rooms/${roomId}/members`)
}

// Messages
export function listMessages(roomId: string, lastSeq?: number, limit?: number) {
  const params = new URLSearchParams()
  if (lastSeq !== undefined) params.set('last_seq', String(lastSeq))
  if (limit !== undefined) params.set('limit', String(limit))
  const qs = params.toString()
  return request<{ messages: import('../types').MessageInfo[] }>(
    `/api/rooms/${roomId}/messages${qs ? `?${qs}` : ''}`,
  )
}

// WebSocket ticket
export function createWsTicket() {
  return request<{ ticket: string }>('/ws-api/ws/ticket', { method: 'POST' })
}
