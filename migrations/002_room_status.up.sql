BEGIN;

CREATE TYPE room_status AS ENUM ('active', 'out_of_service');

ALTER TABLE rooms
    ADD COLUMN status room_status NOT NULL DEFAULT 'active';

COMMIT;
