BEGIN;

CREATE TYPE user_role AS ENUM ('admin', 'manager', 'member');
CREATE TYPE booking_status AS ENUM ('confirmed', 'cancelled');

CREATE TABLE rooms (
    id        TEXT PRIMARY KEY,
    name      TEXT    NOT NULL,
    capacity  INTEGER NOT NULL CHECK (capacity > 0),
    floor     INTEGER NOT NULL,
    equipment TEXT[]  NOT NULL DEFAULT '{}'
);

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    name          TEXT      NOT NULL,
    email         TEXT      NOT NULL UNIQUE,
    role          user_role NOT NULL,
    manages_floor INTEGER
);

CREATE TABLE bookings (
    id         TEXT PRIMARY KEY,
    room_id    TEXT           NOT NULL REFERENCES rooms(id),
    user_id    TEXT           NOT NULL REFERENCES users(id),
    title      TEXT           NOT NULL,
    start_time TIMESTAMPTZ    NOT NULL,
    end_time   TIMESTAMPTZ    NOT NULL,
    status     booking_status NOT NULL DEFAULT 'confirmed',
    CHECK (end_time > start_time)
);

CREATE INDEX idx_bookings_room_status_time ON bookings (room_id, status, start_time, end_time);
CREATE INDEX idx_bookings_user             ON bookings (user_id);
CREATE INDEX idx_bookings_start_time       ON bookings (start_time);

INSERT INTO users (id, name, email, role, manages_floor) VALUES
    ('u-admin',   'Admin User',     'admin@example.com',     'admin',   NULL),
    ('u-mgr-3',   'Manager Floor3', 'manager3@example.com',  'manager', 3),
    ('u-member1', 'Regular Member', 'member1@example.com',   'member',  NULL);

COMMIT;
