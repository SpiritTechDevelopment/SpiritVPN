package app

import (
	"context"
	"fmt"
	"time"
)

// PruneUsageDedup — ретенция реестра дедупа usage-items (§12).
//
// Реестр — единственная таблица, растущая пропорционально трафику: строка на
// активного пользователя каждые 15 секунд. Дедуп без ретенции — это гарантия
// «начислим ровно один раз» ценой неограниченного роста, поэтому §12 явно
// разрешает удалять записи, которые агент заведомо не пришлёт повторно, и
// называет очень поздний повтор после такой очистки принятой положительной
// погрешностью.
//
// Lease нет, в отличие от остальных воркеров, и это не упущение: удалять уже
// удалённую строку нечего, поэтому два прунера, работающих одновременно, дадут
// лишний проход, а не порчу данных. Гейт здесь стоил бы дороже, чем то, от чего
// он защищает.
//
// Доменных решений у прунера нет вовсе: что именно безопасно удалять, знает
// запрос (это утверждение о протоколе спула), а use case задаёт только темп и
// размер пачки.
type PruneUsageDedup struct {
	Repo UsageRetentionRepository

	// Retention — сколько дедуп-записи живут после подтверждения. Окно покрывает
	// ПРОСТОЙ backend: подтверждение может быть записано у нас, но не доехать до
	// агента, и тогда он пришлёт тот же batch снова.
	Retention time.Duration

	// BatchSize — потолок одного шага. Он же признак «работа не закончена»: шаг,
	// удаливший ровно потолок, почти наверняка оставил ещё.
	BatchSize int
}

// NewPruneUsageDedup собирает воркер ретенции.
func NewPruneUsageDedup(repo UsageRetentionRepository, retention time.Duration, batchSize int) *PruneUsageDedup {
	return &PruneUsageDedup{Repo: repo, Retention: retention, BatchSize: batchSize}
}

// ProcessNext удаляет одну пачку.
//
// progressed=true означает «есть что делать прямо сейчас», а не «что-то
// удалено»: неполная пачка означает, что удалять больше нечего, и следующий
// проход должен ждать. Иначе цикл крутился бы без пауз, снося по нескольку
// строк, которые только-только перевалили за окно.
func (uc *PruneUsageDedup) ProcessNext(ctx context.Context) (progressed bool, err error) {
	deleted, err := uc.Repo.PruneProcessedUsageItems(ctx, uc.Retention, uc.BatchSize)
	if err != nil {
		return false, fmt.Errorf("очистка реестра дедупа: %w", err)
	}

	return deleted >= uc.BatchSize, nil
}
