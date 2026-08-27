package domain

import "errors"

// Доменные ошибки. Пакет domain не знает про gRPC; сопоставление с кодами живёт в
// errorMapping (grpcsvc/errors.go) и опирается на эти сентинелы через errors.Is.
var (
	// ErrCustomerIDInvalid — customer_id пуст или длиннее 256 байт.
	ErrCustomerIDInvalid = errors.New("domain: customer_id должен быть непустым и не длиннее 256 байт")

	// ErrFleetIDInvalid — vpn_fleet_id не положителен.
	ErrFleetIDInvalid = errors.New("domain: vpn_fleet_id должен быть > 0")

	// ErrQuotaInvalid — usage_quota_bytes не положителен.
	ErrQuotaInvalid = errors.New("domain: usage_quota_bytes должен быть > 0")

	// ErrCommandNumberInvalid — command_number не положителен.
	ErrCommandNumberInvalid = errors.New("domain: command_number должен быть > 0")

	// ErrExpiryNotInFuture — при создании customer и при renewal момент окончания
	// обязан быть в будущем. Повтор с тем же expiry под это правило не
	// подпадает: он не является renewal и для уже истёкшего customer допустим.
	ErrExpiryNotInFuture = errors.New("domain: expires_at должен быть в будущем")

	// ErrCustomerNotFound — у customer нет корневой строки, то есть ни одного
	// успешного ApplyCustomerAccess: неизвестный customer_id возвращает
	// NOT_FOUND. Это исход read-пути; командный путь такого customer создаёт.
	ErrCustomerNotFound = errors.New("domain: customer не найден")

	// ErrFleetNotFound — fleet отсутствует в текущем manifest.
	// Проверяется вызывающим слоем и только после отсечения устаревшей команды по
	// command_number, иначе повтор старой команды получил бы NOT_FOUND вместо OK.
	ErrFleetNotFound = errors.New("domain: fleet не найден")

	// ErrFleetMismatch — попытка привязать существующего customer к другому fleet.
	// Смена fleet в v1 не поддерживается.
	ErrFleetMismatch = errors.New("domain: customer уже привязан к другому fleet")

	// ErrExpiryRegression — expires_at меньше сохранённого.
	// Сокращение срока в v1 не поддерживается. Устаревшая команда с меньшим
	// expiry отсекается раньше проверкой command_number и сюда не доходит.
	ErrExpiryRegression = errors.New("domain: сокращение expires_at не поддерживается")

	// ErrOpenPeriodMissing — у существующего customer нет открытого периода
	// квоты. Нарушение инварианта «ровно один период с closed_at IS NULL»,
	// а не ошибка вызывающего.
	ErrOpenPeriodMissing = errors.New("domain: у customer нет открытого периода квоты")

	ErrAdministrativeStateInvalid = errors.New("domain: некорректное административное состояние")
	ErrCommandNumberConflict      = errors.New("domain: command_number уже использован другой командой")
	ErrCustomerDeleting           = errors.New("domain: удаление customer ещё не завершено")
)
