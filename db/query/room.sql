-- name: CreateRoom :one
INSERT INTO rooms (
  id, name, manager_id, capacity, created_at
) VALUES (
  $1, $2, $3, $4, $5
) RETURNING id, name, manager_id, capacity, created_at;

-- name: ListJoinedRooms :many
SELECT r.id, r.name, r.manager_id, r.capacity, rm.joined_at,
       (SELECT COUNT(*) FROM room_members WHERE room_id = r.id) AS member_count
FROM rooms r
JOIN room_members rm ON r.id = rm.room_id
WHERE rm.user_id = $1 AND r.deleted_at IS NULL
ORDER BY rm.joined_at DESC;

-- name: SearchRooms :many
SELECT id, name, manager_id, capacity, created_at,
       COUNT(*) OVER() AS total_count,
       (SELECT COUNT(*) FROM room_members WHERE room_id = rooms.id) AS member_count
FROM rooms
WHERE name ILIKE '%' || $1 || '%' AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdateRoom :one
UPDATE rooms
SET name = $2, capacity = $3
WHERE id = $1 AND manager_id = $4 AND deleted_at IS NULL
RETURNING id;

-- name: SoftDeleteRoom :one
UPDATE rooms
SET deleted_at = $3
WHERE id = $1 AND manager_id = $2 AND deleted_at IS NULL
RETURNING id;

-- name: DeleteRoomMember :exec
DELETE FROM room_members
WHERE room_id = $1 AND user_id = $2;

-- name: GetOldestRoomMember :one
SELECT user_id FROM room_members
WHERE room_id = $1
ORDER BY joined_at ASC
LIMIT 1;

-- name: UpdateRoomManager :exec
UPDATE rooms
SET manager_id = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateRoomMember :exec
INSERT INTO room_members (
  user_id, room_id, joined_at
) VALUES (
  $1, $2, $3
) ON CONFLICT (user_id, room_id) DO NOTHING;

-- name: GetRoomMemberCount :one
SELECT COUNT(*) FROM room_members
WHERE room_id = $1;

-- name: GetMemberJoinedAt :one
SELECT rm.joined_at
FROM room_members rm
JOIN rooms r ON r.id = rm.room_id
WHERE rm.room_id = $1 AND rm.user_id = $2 AND r.deleted_at IS NULL
LIMIT 1;

-- name: ExistsRoomMember :one
SELECT EXISTS (
  SELECT 1 FROM room_members rm
  JOIN rooms r ON r.id = rm.room_id
  WHERE rm.room_id = $1 AND rm.user_id = $2 AND r.deleted_at IS NULL
) AS exists;

-- name: ListRoomMembers :many
SELECT u.id, u.username, rm.joined_at
FROM room_members rm
JOIN users u ON u.id = rm.user_id
JOIN rooms r ON r.id = rm.room_id
WHERE rm.room_id = $1 AND r.deleted_at IS NULL
ORDER BY rm.joined_at ASC;

-- name: GetRoomForUpdate :one
SELECT id, name, manager_id, capacity FROM rooms
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE;

-- name: ListJoinedRoomIDsForUpdate :many
SELECT rm.room_id
FROM room_members rm
JOIN rooms r ON r.id = rm.room_id
WHERE rm.user_id = $1 AND r.deleted_at IS NULL
FOR UPDATE OF r;

-- name: PurgeDeletedRooms :execrows
DELETE FROM rooms
WHERE deleted_at IS NOT NULL
  AND deleted_at < $1;
