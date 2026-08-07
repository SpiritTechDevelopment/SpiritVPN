package domain

import "time"

// ApplyDecision — как трактуется принятая команда ApplyCustomerAccess (§5).
type ApplyDecision int

const (
	// ApplyDecisionCreate — первый Apply для customer: создаётся корневая строка
	// и начальный период квоты.
	ApplyDecisionCreate ApplyDecision = iota + 1

	// ApplyDecisionRenewal — expires_at строго больше сохранённого (правило 8):
	// закрывается текущий период, открывается новый с нулевым расходом.
	ApplyDecisionRenewal

	// ApplyDecisionQuotaChange — expires_at совпадает с сохранённым (правило 7):
	// квота может измениться, накопленный трафик не сбрасывается. Если совпадает
	// и квота, план окажется пустым.
	ApplyDecisionQuotaChange
)

func (d ApplyDecision) String() string {
	switch d {
	case ApplyDecisionCreate:
		return "CREATE"
	case ApplyDecisionRenewal:
		return "RENEWAL"
	case ApplyDecisionQuotaChange:
		return "QUOTA_CHANGE"
	default:
		return "UNKNOWN"
	}
}

// ValidateApplyCommand проверяет форму запроса (§5: все поля обязательны,
// vpn_fleet_id > 0, usage_quota_bytes > 0, command_number > 0).
//
// Вызывается ДО обращения к БД: невалидный запрос не должен ни блокировать
// корневую строку, ни двигать last_command_number.
func ValidateApplyCommand(cmd ApplyCommand) error {
	if cmd.CustomerID == "" || len(cmd.CustomerID) > maxCustomerIDBytes {
		return ErrCustomerIDInvalid
	}
	if cmd.FleetID <= 0 {
		return ErrFleetIDInvalid
	}
	if cmd.UsageQuotaBytes == 0 {
		return ErrQuotaInvalid
	}
	if cmd.CommandNumber == 0 {
		return ErrCommandNumberInvalid
	}
	return nil
}

// IsStaleCommand сообщает, что команда переупорядочена или доставлена повторно
// (§5, правило 2): её номер не больше сохранённого. Такая команда завершается
// идемпотентным OK без единого side effect — last_command_number она тоже не
// двигает.
//
// Порядок обязателен: проверка идёт под lock корневой строки (§11.1) и ДО поиска
// fleet. Иначе повтор старой команды для fleet, успевшего исчезнуть из manifest,
// вернул бы NOT_FOUND вместо OK.
//
// Для нового customer (ent == nil) сохранённого номера нет, поэтому устаревшей
// команда быть не может: любой валидный command_number > 0 принимается.
func IsStaleCommand(cmd ApplyCommand, ent *Entitlement) bool {
	if ent == nil {
		return false
	}
	return cmd.CommandNumber <= ent.LastCommandNumber
}

// ClassifyApply определяет род принятой команды по правилам 5, 8 и 9 §5.
//
// Предусловия, обеспечиваемые вызывающим слоем: команда прошла
// ValidateApplyCommand, не является устаревшей по IsStaleCommand, и fleet
// существует в текущем manifest.
func ClassifyApply(now time.Time, cmd ApplyCommand, ent *Entitlement) (ApplyDecision, error) {
	// Новый customer: создание требует момента окончания в будущем (§5).
	if ent == nil {
		if !cmd.ExpiresAt.After(now) {
			return 0, ErrExpiryNotInFuture
		}
		return ApplyDecisionCreate, nil
	}

	// Правило 5: смена fleet существующего customer в v1 не поддерживается.
	if ent.FleetID != cmd.FleetID {
		return 0, ErrFleetMismatch
	}

	switch {
	// Правило 9: сокращение срока отклоняется.
	case cmd.ExpiresAt.Before(ent.ExpiresAt):
		return 0, ErrExpiryRegression

	// Правило 8: renewal. Требование «в будущем» здесь не избыточно: у уже
	// истёкшего customer более поздний expires_at всё ещё может лежать в прошлом.
	case cmd.ExpiresAt.After(ent.ExpiresAt):
		if !cmd.ExpiresAt.After(now) {
			return 0, ErrExpiryNotInFuture
		}
		return ApplyDecisionRenewal, nil

	// Правило 7: тот же expiry — изменение квоты без сброса трафика. Момента в
	// будущем не требует: истёкший customer вправе повторить свою команду, просто
	// его access останутся ABSENT.
	default:
		return ApplyDecisionQuotaChange, nil
	}
}
