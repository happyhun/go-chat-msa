CREATE TABLE users (
  id uuid PRIMARY KEY,
  username text NOT NULL UNIQUE,
  password_hash text NOT NULL,
  created_at timestamptz NOT NULL
);

CREATE TABLE rooms (
  id uuid PRIMARY KEY,
  name text NOT NULL,
  manager_id uuid REFERENCES users(id) ON DELETE SET NULL,
  capacity int NOT NULL,
  created_at timestamptz NOT NULL,
  deleted_at timestamptz NULL
);

CREATE INDEX idx_rooms_manager_id ON rooms(manager_id);

CREATE TABLE room_members (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  room_id uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  joined_at timestamptz NOT NULL,
  PRIMARY KEY (user_id, room_id)
);

CREATE INDEX idx_room_members_room_id ON room_members(room_id);

CREATE TABLE refresh_tokens (
  id         uuid        PRIMARY KEY,
  user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash text        NOT NULL,
  used       boolean     NOT NULL DEFAULT false,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL
);

CREATE INDEX idx_refresh_tokens_user_id ON refresh_tokens(user_id);
CREATE INDEX idx_refresh_tokens_token_hash ON refresh_tokens(token_hash);

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX idx_rooms_name_trgm ON rooms USING gin (name gin_trgm_ops);
