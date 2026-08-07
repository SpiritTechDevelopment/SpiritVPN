package migrations

import (
	"io"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// TestEmbeddedSourceLoads проверяет, что встроенные миграции видны источнику iofs и
// что базовая версия отдаёт и up-, и down-миграцию. Ловит пропавший/переименованный
// .sql-файл без обращения к базе.
func TestEmbeddedSourceLoads(t *testing.T) {
	src, err := iofs.New(files, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	defer src.Close()

	first, err := src.First()
	if err != nil {
		t.Fatalf("First: %v", err)
	}
	if first != 1 {
		t.Fatalf("первая версия миграции = %d, ожидалось 1", first)
	}

	// До базовой версии никакой другой быть не должно.
	if _, err := src.Prev(first); err == nil {
		t.Fatalf("Prev(%d) неожиданно успешен; baseline должен быть первым", first)
	}

	up := readMigration(t, func() (io.ReadCloser, string, error) { return src.ReadUp(first) })
	down := readMigration(t, func() (io.ReadCloser, string, error) { return src.ReadDown(first) })

	// Up обязан создать корневую таблицу customer и once-only реестр учёта.
	for _, want := range []string{
		"CREATE TABLE customer_entitlements",
		"CREATE TABLE vpn_accesses",
		"CREATE TABLE agent_operations",
		"CREATE TABLE traffic_usage_items_processed",
		"GENERATED ALWAYS AS (uplink_bytes + downlink_bytes) STORED",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("в up-миграции нет %q", want)
		}
	}

	// Down обязан удалить базовые таблицы.
	for _, want := range []string{
		"DROP TABLE IF EXISTS customer_entitlements",
		"DROP TABLE IF EXISTS manifest_revisions",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("в down-миграции нет %q", want)
		}
	}
}

func readMigration(t *testing.T, open func() (io.ReadCloser, string, error)) string {
	t.Helper()
	rc, _, err := open()
	if err != nil {
		t.Fatalf("открыть миграцию: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("прочитать миграцию: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("миграция пустая")
	}
	return string(b)
}
