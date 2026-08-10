package domain

import "errors"

// Доменные ошибки. Пакет domain не знает про gRPC; сопоставление с кодами живёт в
// grpcsvc и опирается на эти сентинелы через errors.Is.
//
// Ожидаемое сопоставление (§5):
//
//	INVALID_ARGUMENT     — все ошибки валидации запроса и ErrExpiryNotInFuture
//	NOT_FOUND            — ErrFleetNotFound (правило 6), ErrCustomerNotFound
//	FAILED_PRECONDITION  — ErrFleetMismatch (правило 5), ErrExpiryRegression (правило 9)
//	INTERNAL             — ErrOpenPeriodMissing
var (
	// ErrCustomerIDInvalid — customer_id пуст или длиннее 256 байт (§3).
	ErrCustomerIDInvalid = errors.New("domain: customer_id должен быть непустым и не длиннее 256 байт")

	// ErrFleetIDInvalid — vpn_fleet_id не положителен (§5).
	ErrFleetIDInvalid = errors.New("domain: vpn_fleet_id должен быть > 0")

	// ErrQuotaInvalid — usage_quota_bytes не положителен (§5).
	ErrQuotaInvalid = errors.New("domain: usage_quota_bytes должен быть > 0")

	// ErrCommandNumberInvalid — command_number не положителен (§5).
	ErrCommandNumberInvalid = errors.New("domain: command_number должен быть > 0")

	// ErrExpiryNotInFuture — при создании customer и при renewal момент окончания
	// обязан быть в будущем (§5). Повтор с тем же expiry под это правило не
	// подпадает: он не является renewal и для уже истёкшего customer допустим.
	ErrExpiryNotInFuture = errors.New("domain: expires_at должен быть в будущем")

	// ErrCustomerNotFound — у customer нет корневой строки, то есть ни одного
	// успешного ApplyCustomerAccess (§5: неизвестный customer_id возвращает
	// NOT_FOUND). Это исход read-пути; командный путь такого customer создаёт.
	ErrCustomerNotFound = errors.New("domain: customer не найден")

	// ErrFleetNotFound — fleet отсутствует в текущем manifest (§5, правило 6).
	// Проверяется вызывающим слоем и только ПОСЛЕ отсечения устаревшей команды по
	// command_number, иначе повтор старой команды получил бы NOT_FOUND вместо OK.
	ErrFleetNotFound = errors.New("domain: fleet не найден")

	// ErrFleetMismatch — попытка привязать существующего customer к другому fleet
	// (§5, правило 5). Смена fleet в v1 не поддерживается.
	ErrFleetMismatch = errors.New("domain: customer уже привязан к другому fleet")

	// ErrExpiryRegression — expires_at меньше сохранённого (§5, правило 9).
	// Сокращение срока в v1 не поддерживается. Устаревшая команда с меньшим
	// expiry отсекается раньше правилом 2 по command_number и сюда не доходит.
	ErrExpiryRegression = errors.New("domain: сокращение expires_at не поддерживается")

	// ErrOpenPeriodMissing — у существующего customer нет открытого периода
	// квоты. Нарушение инварианта §11 (ровно один период с closed_at IS NULL),
	// а не ошибка вызывающего.
	ErrOpenPeriodMissing = errors.New("domain: у customer нет открытого периода квоты")
)
