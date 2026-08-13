package grpcsvc

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// requestIDMetadataKey — ключ metadata, по которому вызывающий может передать
// свой идентификатор запроса. Имя общепринятое, чтобы request_id совпадал со
// сквозным идентификатором product-сервиса и обе стороны искали по одному
// значению.
const requestIDMetadataKey = "x-request-id"

// maxRequestIDLen ограничивает то, что попадёт в каждую запись лога.
const maxRequestIDLen = 64

// RequestIDUnaryInterceptor кладёт в контекст идентификатор запроса.
//
// Значение берётся из metadata вызывающего, если оно пригодно, иначе
// генерируется. Приоритет у входящего именно для сквозной корреляции: одна
// команда product-сервиса и её обработка здесь должны находиться одним поиском.
//
// newID вызывается только когда своего идентификатора нет; параметром, а не
// жёстко зашитым uuid, чтобы тест мог сделать значение предсказуемым.
func RequestIDUnaryInterceptor(newID func() string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		_ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		return handler(contextWithRequestID(ctx, requestID(ctx, newID)), req)
	}
}

func requestID(ctx context.Context, newID func() string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		for _, raw := range md.Get(requestIDMetadataKey) {
			if id, valid := sanitizeRequestID(raw); valid {
				return id
			}
		}
	}
	return newID()
}

// sanitizeRequestID пропускает только то, что безопасно положить в лог.
//
// Значение приходит из metadata и полностью контролируется вызывающим. Перевод
// строки внутри него разорвал бы JSON-запись лога на две, и вторая половина
// выглядела бы как отдельное самостоятельное событие — то есть вызывающий мог бы
// подделывать записи в чужом логе. Поэтому пропускаются только печатные ASCII
// без пробелов, а длина ограничена: неограниченный идентификатор попадает в
// КАЖДУЮ запись запроса и раздувает лог линейно.
//
// Непригодное значение не отвергает запрос, а молча заменяется сгенерированным:
// корреляция — удобство наблюдаемости, а не правило контракта, и ронять из-за неё
// команду product-сервиса неправильно.
func sanitizeRequestID(raw string) (string, bool) {
	if raw == "" || len(raw) > maxRequestIDLen {
		return "", false
	}

	for _, r := range raw {
		if r < '!' || r > '~' {
			return "", false
		}
	}
	return raw, true
}
