package grpcsvc

import "context"

// contextKey — приватный тип ключа: значения этого пакета не должны читаться и
// перезаписываться чужим кодом, положившим в контекст строку с тем же именем.
type contextKey int

const (
	requestIDContextKey contextKey = iota
)

// contextWithRequestID кладёт идентификатор запроса в контекст.
func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey, id)
}

// RequestIDFromContext возвращает идентификатор запроса или пустую строку.
//
// Экспортирован намеренно: §15 требует request_id в structured logs, а логируют
// не только interceptor'ы — до него доберутся и трейсер pgx, и будущий audit.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey).(string)
	return id
}
