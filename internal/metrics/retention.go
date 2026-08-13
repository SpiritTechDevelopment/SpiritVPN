package metrics

import (
	"context"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// WrapUsageRetention считает удалённые дедуп-записи.
//
// Счётчик отвечает на вопрос «прунер вообще работает», тогда как парный ему
// gauge возраста самой старой записи отвечает на «справляется ли». Порознь оба
// врут: возраст держится около окна ретенции и при живом прунере, и при
// прунере, который только что умер, а ненулевой rate счётчика ничего не говорит
// о том, успевает ли уборка за темпом записи.
//
// Декоратор порта, а не счётчик внутри use case: про Prometheus не знает ни
// один пакет, кроме этого, и ретенция не повод заводить исключение.
func (r *Registry) WrapUsageRetention(inner app.UsageRetentionRepository) app.UsageRetentionRepository {
	return &observedUsageRetention{inner: inner, reg: r}
}

type observedUsageRetention struct {
	inner app.UsageRetentionRepository
	reg   *Registry
}

func (o *observedUsageRetention) PruneProcessedUsageItems(
	ctx context.Context,
	retention time.Duration,
	limit int,
) (int, error) {
	deleted, err := o.inner.PruneProcessedUsageItems(ctx, retention, limit)

	// Отказавший шаг мог удалить часть пачки только вместе с откатом своей
	// транзакции, поэтому считается ровно успех.
	if err == nil {
		o.reg.usageDedupPruned.Add(float64(deleted))
	}
	return deleted, err
}
