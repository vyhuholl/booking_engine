package testutil

import (
	"bytes"
	"log/slog"
	"sync"
)

// LogBuffer — потокобезопасный приёмник записей slog для проверок в тестах.
// Мьютекс нужен на случай, если операция логирует из фоновой горутины.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String возвращает накопленный вывод логгера.
func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// CaptureLogger возвращает slog-логгер, пишущий JSON в отдаваемый буфер (уровень
// Debug и выше). Используется, чтобы убедиться, что операция залогировала нужное
// событие — например деградацию Kafka/Redis на уровне Warn.
func CaptureLogger() (*slog.Logger, *LogBuffer) {
	buf := &LogBuffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}
