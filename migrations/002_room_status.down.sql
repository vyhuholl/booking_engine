BEGIN;

ALTER TABLE rooms DROP COLUMN status;
DROP TYPE room_status;

COMMIT;
