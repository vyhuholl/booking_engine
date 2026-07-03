-- Колонки и индекс workflow одобрения (change add-large-room-approval).
-- Отдельно от 004_booking_status_values: партиальный индекс ниже ссылается на
-- значение enum 'pending_approval', которое обязано быть закоммичено ранее.
BEGIN;

-- Причина отклонения брони: заполняется при reject/авто-reject, NULL для остальных.
ALTER TABLE bookings ADD COLUMN rejection_reason TEXT;

-- Момент создания брони — якорь 24-часового таймаута одобрения. DEFAULT now()
-- бэкфиллит существующие строки временем миграции; это безопасно, так как таймаут
-- касается только новых pending_approval-броней (см. design.md, Решение 5).
ALTER TABLE bookings ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Быстрая выборка и «подметание» броней, ожидающих одобрения: список admin-очереди
-- (GET /admin/approvals) и ленивый авто-reject по таймауту. Партиальный — индексирует
-- только немногочисленные pending_approval-строки (ср. uq_waitlist_active в 003).
CREATE INDEX idx_bookings_pending_created ON bookings (status, created_at)
    WHERE status = 'pending_approval';

COMMIT;
