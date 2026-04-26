ALTER TABLE users ADD COLUMN deleted_at timestamptz NULL;

CREATE INDEX idx_users_deleted_at ON users(deleted_at) WHERE deleted_at IS NOT NULL;
