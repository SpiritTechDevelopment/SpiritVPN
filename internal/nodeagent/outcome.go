package nodeagent

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// Стабильные коды исхода для логов и метрик.
//
// Отделены от gRPC-кода и от текста агента: gRPC-код слишком груб (пять разных
// причин делят permanent), а текст приходит с чужой стороны и меняется вместе с
// её версией. По этим кодам строятся alert'ы, поэтому переименование любого —
// изменение контракта наблюдаемости.
const (
	CodeApplied           = "APPLIED"
	CodeAlreadyApplied    = "ALREADY_APPLIED"
	CodeAgentRetryable    = "AGENT_RETRYABLE"
	CodeAgentPermanent    = "AGENT_PERMANENT"
	CodeAgentUnknown      = "AGENT_UNKNOWN_STATUS"
	CodeUnavailable       = "UNAVAILABLE"
	CodeDeadline          = "DEADLINE_EXCEEDED"
	CodeAborted           = "ABORTED"
	CodeInvalidArgument   = "INVALID_ARGUMENT"
	CodePrecondition      = "FAILED_PRECONDITION"
	CodeUnauthenticated   = "UNAUTHENTICATED"
	CodePermissionDenied  = "PERMISSION_DENIED"
	CodeUnimplemented     = "UNIMPLEMENTED"
	CodeIdentityMismatch  = "IDENTITY_MISMATCH"
	CodeNodeConfigInvalid = "NODE_CONFIG_INVALID"
	CodeTransport         = "TRANSPORT"
)

// Outcome — исход одной попытки доставки.
//
// Ошибки метод не возвращает: любой отказ уже классифицирован здесь, и
// вызывающему всегда есть что записать в agent_operations. Разделение на
// (Outcome, error) заставило бы диспетчер классифицировать транспорт второй раз.
type Outcome struct {
	Result domain.AttemptOutcome
	// Code — стабильный код для наблюдаемости.
	Code string
	// Message — санитизированная диагностика агента либо текст ошибки транспорта.
	// Секретов не содержит: агент их вычищает, а backend ничего своего сюда не
	// добавляет.
	Message string
	// Alert отмечает исход, который поднимается оператору: подмена
	// идентичности и неопознанные transport/internal ошибки.
	Alert bool
}

// classify сводит два канала ответа в один исход.
//
// Агент отвечает и gRPC-кодом, и ApplyStatus в теле, и они независимы: gRPC OK с
// RETRYABLE_ERROR внутри — штатный ответ «сейчас не могу», а не успех.
func classify(result *nodeagentv1.OperationResult, err error) Outcome {
	if err != nil {
		return classifyTransport(err)
	}

	message := result.GetMessage()

	switch result.GetStatus() {
	case nodeagentv1.ApplyStatus_APPLY_STATUS_APPLIED:
		return Outcome{Result: domain.AttemptSucceeded, Code: CodeApplied, Message: message}

	case nodeagentv1.ApplyStatus_APPLY_STATUS_ALREADY_APPLIED:
		// Уже точное состояние возвращает OK. Идемпотентность повтора —
		// именно то, на чём держится безопасность потерянного ответа.
		return Outcome{Result: domain.AttemptSucceeded, Code: CodeAlreadyApplied, Message: message}

	case nodeagentv1.ApplyStatus_APPLY_STATUS_RETRYABLE_ERROR:
		return Outcome{Result: domain.AttemptRetryable, Code: CodeAgentRetryable, Message: message}

	case nodeagentv1.ApplyStatus_APPLY_STATUS_PERMANENT_ERROR:
		return Outcome{Result: domain.AttemptPermanent, Code: CodeAgentPermanent, Message: message}

	default:
		// UNSPECIFIED при gRPC OK. Агент не назвал исход, значит договориться с
		// ним нельзя: повтор ничего не даст, и это дефект его версии — alert.
		return Outcome{
			Result:  domain.AttemptPermanent,
			Code:    CodeAgentUnknown,
			Message: message,
			Alert:   true,
		}
	}
}

// classifyTransport разбирает ошибку вызова по таблице транспортных кодов.
func classifyTransport(err error) Outcome {
	// Подмена идентичности проверяется первой: она приезжает завёрнутой в
	// transport-ошибку рукопожатия, и по одному коду её от обычной недоступности
	// не отличить. Ретраить её нельзя — это security failure, а не сбой сети.
	if errors.Is(err, ErrIdentityMismatch) {
		return Outcome{
			Result:  domain.AttemptPermanent,
			Code:    CodeIdentityMismatch,
			Message: err.Error(),
			Alert:   true,
		}
	}

	// Непригодный agent_config — retryable, а не permanent: чинится
	// он следующим манифестом, который desired_version access не двигает и новой
	// операции не создаёт. Permanent означал бы, что чинить уже нечего.
	if errors.Is(err, ErrEndpointIncomplete) {
		return Outcome{
			Result:  domain.AttemptRetryable,
			Code:    CodeNodeConfigInvalid,
			Message: err.Error(),
			Alert:   true,
		}
	}

	switch status.Code(err) {
	case codes.Unavailable:
		return Outcome{Result: domain.AttemptRetryable, Code: CodeUnavailable, Message: err.Error()}
	case codes.DeadlineExceeded:
		return Outcome{Result: domain.AttemptRetryable, Code: CodeDeadline, Message: err.Error()}
	case codes.Aborted:
		return Outcome{Result: domain.AttemptRetryable, Code: CodeAborted, Message: err.Error()}

	case codes.InvalidArgument:
		return Outcome{Result: domain.AttemptPermanent, Code: CodeInvalidArgument, Message: err.Error()}
	case codes.FailedPrecondition:
		return Outcome{Result: domain.AttemptPermanent, Code: CodePrecondition, Message: err.Error()}
	case codes.Unauthenticated:
		return Outcome{Result: domain.AttemptPermanent, Code: CodeUnauthenticated, Message: err.Error(), Alert: true}
	case codes.PermissionDenied:
		return Outcome{Result: domain.AttemptPermanent, Code: CodePermissionDenied, Message: err.Error(), Alert: true}
	case codes.Unimplemented:
		return Outcome{Result: domain.AttemptPermanent, Code: CodeUnimplemented, Message: err.Error(), Alert: true}

	default:
		// Прочие transport/internal ошибки повторяются с backoff и создают
		// alert. Ретраится, потому что неизвестное скорее временно, чем нет;
		// alert — потому что неизвестное не должно тихо копиться.
		return Outcome{Result: domain.AttemptRetryable, Code: CodeTransport, Message: err.Error(), Alert: true}
	}
}
