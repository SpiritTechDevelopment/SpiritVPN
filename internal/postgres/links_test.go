package postgres

import (
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// TestLinkSourceFromRowPreservesQuotaRange фиксирует полный uint64-диапазон на
// новом read-пути: sqlc отдаёт numeric, а наружу должны доехать те же байты.
func TestLinkSourceFromRowPreservesQuotaRange(t *testing.T) {
	source, err := linkSourceFromRow(db.ListCustomerAccessLinksRow{
		UsageQuotaBytes: numericFromUint64(math.MaxUint64),
		ConsumedBytes:   numericFromUint64(math.MaxUint64 - 1),
	})
	if err != nil {
		t.Fatalf("linkSourceFromRow: %v", err)
	}
	if source.UsageQuotaBytes != math.MaxUint64 {
		t.Errorf("quota=%d, ожидалось %d", source.UsageQuotaBytes, uint64(math.MaxUint64))
	}
	if source.ConsumedBytes != math.MaxUint64-1 {
		t.Errorf("consumed=%d, ожидалось %d", source.ConsumedBytes, uint64(math.MaxUint64-1))
	}
}

// TestLinkSourceFromRowRejectsInvalidQuota не позволяет повреждённому numeric
// молча превратиться в нулевой расход или лимит в Customer API.
func TestLinkSourceFromRowRejectsInvalidQuota(t *testing.T) {
	_, err := linkSourceFromRow(db.ListCustomerAccessLinksRow{
		UsageQuotaBytes: pgtype.Numeric{},
		ConsumedBytes:   numericFromUint64(0),
	})
	if !errors.Is(err, ErrNumericOutOfRange) {
		t.Fatalf("ошибка %v, ожидалась ErrNumericOutOfRange", err)
	}
}
