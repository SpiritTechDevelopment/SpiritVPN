package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Общая шкала времени тестов домена: tNow — момент now() транзакции.
var (
	tNow    = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tFuture = tNow.Add(24 * time.Hour)
	tLater  = tNow.Add(48 * time.Hour)
	tPast   = tNow.Add(-24 * time.Hour)
)

func validCommand() ApplyCommand {
	return ApplyCommand{
		CustomerID:      "cust-1",
		FleetID:         10,
		UsageQuotaBytes: 1 << 30,
		ExpiresAt:       tFuture,
		CommandNumber:   1,
	}
}

func TestValidateApplyCommand(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ApplyCommand)
		wantErr error
	}{
		{
			name:   "валидная команда",
			mutate: func(*ApplyCommand) {},
		},
		{
			name:    "пустой customer_id",
			mutate:  func(c *ApplyCommand) { c.CustomerID = "" },
			wantErr: ErrCustomerIDInvalid,
		},
		{
			name:   "customer_id ровно 256 байт — граница допустима",
			mutate: func(c *ApplyCommand) { c.CustomerID = strings.Repeat("x", 256) },
		},
		{
			name:    "customer_id 257 байт",
			mutate:  func(c *ApplyCommand) { c.CustomerID = strings.Repeat("x", 257) },
			wantErr: ErrCustomerIDInvalid,
		},
		{
			name:    "нулевой fleet",
			mutate:  func(c *ApplyCommand) { c.FleetID = 0 },
			wantErr: ErrFleetIDInvalid,
		},
		{
			name:    "отрицательный fleet",
			mutate:  func(c *ApplyCommand) { c.FleetID = -1 },
			wantErr: ErrFleetIDInvalid,
		},
		{
			name:    "нулевая квота",
			mutate:  func(c *ApplyCommand) { c.UsageQuotaBytes = 0 },
			wantErr: ErrQuotaInvalid,
		},
		{
			name:    "нулевой command_number",
			mutate:  func(c *ApplyCommand) { c.CommandNumber = 0 },
			wantErr: ErrCommandNumberInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validCommand()
			tt.mutate(&cmd)

			err := ValidateApplyCommand(cmd)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateApplyCommand() = %v, ожидалось %v", err, tt.wantErr)
			}
		})
	}
}

// Команда с номером не больше сохранённого поглощается без side
// effects. Правило симметрично защищает и от переупорядочивания, и от повтора.
func TestIsStaleCommand(t *testing.T) {
	tests := []struct {
		name          string
		commandNumber uint64
		entitlement   *Entitlement
		want          bool
	}{
		{
			name:          "новый customer — сохранённого номера нет",
			commandNumber: 1,
			entitlement:   nil,
			want:          false,
		},
		{
			name:          "номер строго больше — принимается",
			commandNumber: 8,
			entitlement:   &Entitlement{LastCommandNumber: 7},
			want:          false,
		},
		{
			name:          "равный номер — повтор доставки",
			commandNumber: 7,
			entitlement:   &Entitlement{LastCommandNumber: 7},
			want:          true,
		},
		{
			name:          "меньший номер — переупорядоченная команда",
			commandNumber: 3,
			entitlement:   &Entitlement{LastCommandNumber: 7},
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validCommand()
			cmd.CommandNumber = tt.commandNumber

			if got := IsStaleCommand(cmd, tt.entitlement); got != tt.want {
				t.Fatalf("IsStaleCommand() = %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

// Классификация принятой команды: создание, продление, смена квоты.
func TestClassifyApply(t *testing.T) {
	tests := []struct {
		name         string
		expiresAt    time.Time
		fleetID      int64
		entitlement  *Entitlement
		wantDecision ApplyDecision
		wantErr      error
	}{
		{
			name:         "новый customer с моментом в будущем",
			expiresAt:    tFuture,
			fleetID:      10,
			entitlement:  nil,
			wantDecision: ApplyDecisionCreate,
		},
		{
			name:        "новый customer с моментом в прошлом",
			expiresAt:   tPast,
			fleetID:     10,
			entitlement: nil,
			wantErr:     ErrExpiryNotInFuture,
		},
		{
			name:        "новый customer с моментом ровно now — не в будущем",
			expiresAt:   tNow,
			fleetID:     10,
			entitlement: nil,
			wantErr:     ErrExpiryNotInFuture,
		},
		{
			name:        "другой fleet — правило 5",
			expiresAt:   tFuture,
			fleetID:     11,
			entitlement: &Entitlement{FleetID: 10, ExpiresAt: tFuture},
			wantErr:     ErrFleetMismatch,
		},
		{
			name:        "сокращение срока — правило 9",
			expiresAt:   tFuture,
			fleetID:     10,
			entitlement: &Entitlement{FleetID: 10, ExpiresAt: tLater},
			wantErr:     ErrExpiryRegression,
		},
		{
			name:         "более поздний срок — renewal, правило 8",
			expiresAt:    tLater,
			fleetID:      10,
			entitlement:  &Entitlement{FleetID: 10, ExpiresAt: tFuture},
			wantDecision: ApplyDecisionRenewal,
		},
		{
			name:      "renewal истёкшего customer в прошлое — всё ещё не в будущем",
			expiresAt: tPast.Add(time.Hour),
			fleetID:   10,
			// Срок больше сохранённого, но оба лежат в прошлом.
			entitlement: &Entitlement{FleetID: 10, ExpiresAt: tPast},
			wantErr:     ErrExpiryNotInFuture,
		},
		{
			name:         "тот же срок — изменение квоты, правило 7",
			expiresAt:    tFuture,
			fleetID:      10,
			entitlement:  &Entitlement{FleetID: 10, ExpiresAt: tFuture},
			wantDecision: ApplyDecisionQuotaChange,
		},
		{
			name:      "тот же срок у истёкшего customer — не ошибка",
			expiresAt: tPast,
			fleetID:   10,
			// Требование «в будущем» относится только к созданию и renewal.
			entitlement:  &Entitlement{FleetID: 10, ExpiresAt: tPast},
			wantDecision: ApplyDecisionQuotaChange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validCommand()
			cmd.ExpiresAt = tt.expiresAt
			cmd.FleetID = tt.fleetID

			got, err := ClassifyApply(tNow, cmd, tt.entitlement)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ClassifyApply() ошибка = %v, ожидалось %v", err, tt.wantErr)
			}
			if got != tt.wantDecision {
				t.Fatalf("ClassifyApply() = %v, ожидалось %v", got, tt.wantDecision)
			}
		})
	}
}
