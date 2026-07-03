BEGIN;

-- Идемпотентность обработки событий нотификатором: event_id уже обработанного
-- события. Повторная доставка того же события (at-least-once) не порождает
-- повторных уведомлений — см. internal/notifications.Dispatcher.
CREATE TABLE processed_events (
    event_id     TEXT        PRIMARY KEY,
    event_type   TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Уведомления, не доставленные после исчерпания ретраев: операционный разбор
-- вручную. Одно «отравленное» уведомление не блокирует очередь.
CREATE TABLE notification_dead_letter (
    id                BIGSERIAL   PRIMARY KEY,
    event_id          TEXT        NOT NULL,
    user_id           TEXT        NOT NULL,
    notification_type TEXT        NOT NULL,
    error             TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
