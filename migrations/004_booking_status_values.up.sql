-- Расширяем enum booking_status значениями workflow одобрения больших переговорок
-- (change add-large-room-approval).
--
-- ВАЖНО: добавление значений enum вынесено в ОТДЕЛЬНУЮ миграцию — до их
-- использования. PostgreSQL запрещает использовать только что добавленное значение
-- enum в той же транзакции ("unsafe use of new value"), а партиальный индекс в
-- 005_booking_approval_columns ссылается на 'pending_approval'. Значения должны быть
-- закоммичены раньше, поэтому они здесь и только здесь.
BEGIN;

ALTER TYPE booking_status ADD VALUE IF NOT EXISTS 'pending_approval';
ALTER TYPE booking_status ADD VALUE IF NOT EXISTS 'approved';
ALTER TYPE booking_status ADD VALUE IF NOT EXISTS 'rejected';

COMMIT;
