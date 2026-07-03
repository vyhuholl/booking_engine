BEGIN;

DROP INDEX IF EXISTS idx_bookings_pending_created;
ALTER TABLE bookings DROP COLUMN IF EXISTS created_at;
ALTER TABLE bookings DROP COLUMN IF EXISTS rejection_reason;

COMMIT;
