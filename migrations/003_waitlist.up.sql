BEGIN;

CREATE TYPE waitlist_status AS ENUM ('waiting', 'offered', 'expired', 'converted');

CREATE TABLE waitlist_entries (
    id         TEXT PRIMARY KEY,
    room_id    TEXT            NOT NULL REFERENCES rooms(id),
    user_id    TEXT            NOT NULL REFERENCES users(id),
    start_time TIMESTAMPTZ     NOT NULL,
    end_time   TIMESTAMPTZ     NOT NULL,
    position   INTEGER         NOT NULL,
    status     waitlist_status NOT NULL DEFAULT 'waiting',
    offered_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ     NOT NULL DEFAULT now(),
    CHECK (end_time > start_time)
);

-- Инвариант: один пользователь — одна активная (waiting/offered) запись на интервал
-- комнаты. После expired/converted пользователь может встать в очередь заново.
CREATE UNIQUE INDEX uq_waitlist_active
    ON waitlist_entries (room_id, user_id, start_time, end_time)
    WHERE status IN ('waiting', 'offered');

-- Выборка очереди комнаты и поиск следующего кандидата для предложения слота.
CREATE INDEX idx_waitlist_room_status_pos ON waitlist_entries (room_id, status, position);

-- Поиск пересекающихся записей при отмене брони.
CREATE INDEX idx_waitlist_room_interval ON waitlist_entries (room_id, start_time, end_time);

COMMIT;
