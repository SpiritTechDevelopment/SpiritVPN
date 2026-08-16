package grpcsvc

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LoggingUnaryInterceptor пишет по одной записи на вызов.
//
// Запись содержит request_id, стабильный error code и идентичность вызывающего.
// operation_id и node_id нужны тоже, но они принадлежат не RPC, а
// operation dispatcher'у, и появятся в его логах — здесь их взять неоткуда.
//
// Тело запроса не логируется никогда: в нём customer_id, а он допустим
// только в ограниченных audit records. По той же причине не логируется и ответ —
// GetCustomerAccessLinks вернёт VLESS URI с credentials внутри.
func LoggingUnaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		started := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		logger.LogAttrs(ctx, levelFor(code), "grpc",
			slog.String("method", info.FullMethod),
			slog.String("grpc_code", code.String()),
			slog.String("error_code", stableCode(err)),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			slog.String("request_id", RequestIDFromContext(ctx)),
			slog.String("peer_identity", peerIdentity(ctx)),
		)

		return resp, err
	}
}

// levelFor разводит исходы по уровням так, чтобы error означал «разбирайся».
//
// Отказ по правилам контракта — штатная работа сервиса, а не поломка: продление с
// сокращением срока или неизвестный fleet происходят из-за того, что прислал
// вызывающий. На error остаётся только то, в чём виноват backend.
func levelFor(code codes.Code) slog.Level {
	switch code {
	case codes.OK:
		return slog.LevelInfo
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// peerIdentity возвращает первую идентичность вызывающего для лога.
//
// Берётся независимо от Authorizer, а не из контекста после него: логируется в
// том числе отказ авторизации, и «кому именно отказали» — самое ценное в такой
// записи. Значение уже проверено цепочкой сертификатов, подделать его нельзя.
func peerIdentity(ctx context.Context) string {
	if ids := peerIdentities(ctx); len(ids) > 0 {
		return ids[0]
	}
	return ""
}
