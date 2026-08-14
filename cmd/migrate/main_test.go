package main

import (
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// Тесты команды миграций. Она запускается первым шагом деплоя, до старта
// spiritvpnd, и её отказ останавливает выкатку целиком.
//
// Здесь проверяются сборка строки подключения и трактовка исходов, то есть всё,
// что решается до первого запроса. Ветки run идут против настоящей базы в
// run_integration_test.go: разбор команды в нём происходит уже после подключения,
// и без базы такой тест проверял бы только недоступность хоста.

// capture перехватывает stdout и stderr на время вызова fn.
//
// Перехват нужен, а не избыточен: report возвращает nil и при успехе, и при
// migrate.ErrNoChange, и различить эти два исхода можно только по напечатанному.
// Тест без перехвата принимал бы любую из двух веток за другую.
//
// Чтение идёт в отдельных горутинах: буфер канала ограничен, и печать длиннее
// него заблокировала бы fn на записи.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	outCh, errCh := make(chan string, 1), make(chan string, 1)
	drain := func(r *os.File, ch chan<- string) {
		data, _ := io.ReadAll(r)
		ch <- string(data)
	}
	go drain(outR, outCh)
	go drain(errR, errCh)

	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = savedOut, savedErr
	}()

	fn()

	os.Stdout, os.Stderr = savedOut, savedErr
	outW.Close()
	errW.Close()

	return <-outCh, <-errCh
}

// clearEnv гасит все переменные подключения: пустая строка для dsn равнозначна
// незаданной.
//
// Гасятся именно все. Интеграционный прогон задаёт DATABASE_URL на весь процесс,
// и без этого тесты сборки из частей читали бы его вместо своих значений.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"DATABASE_URL",
		"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE",
	} {
		t.Setenv(key, "")
	}
}

// DATABASE_URL уезжает в драйвер как есть. Разобрать и пересобрать его нельзя:
// в нём могут стоять параметры, которых сборка из частей не знает.
func TestDSNPrefersDatabaseURL(t *testing.T) {
	clearEnv(t)

	const want = "postgres://u:p@example.internal:6432/db?sslmode=verify-full&application_name=migrate"
	t.Setenv("DATABASE_URL", want)
	// Части заданы и обязаны быть проигнорированы целиком.
	t.Setenv("DB_HOST", "wrong.example")
	t.Setenv("DB_NAME", "wrong")

	if got := dsn(); got != want {
		t.Errorf("dsn() = %q, ожидался нетронутый DATABASE_URL %q", got, want)
	}
}

// Значения по умолчанию являются контрактом с docker-compose.dev.yml.
func TestDSNDefaultsWithoutEnv(t *testing.T) {
	clearEnv(t)

	u, err := url.Parse(dsn())
	if err != nil {
		t.Fatalf("dsn() выдал неразбираемый URL: %v", err)
	}

	if u.Scheme != "postgres" {
		t.Errorf("схема %q, ожидалась postgres", u.Scheme)
	}
	if u.Host != "localhost:5432" {
		t.Errorf("host %q, ожидался localhost:5432", u.Host)
	}
	if u.User.Username() != "spiritdb" {
		t.Errorf("пользователь %q, ожидался spiritdb", u.User.Username())
	}
	if u.Path != "/spiritdb" {
		t.Errorf("база %q, ожидалась /spiritdb", u.Path)
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode %q, ожидался disable", got)
	}
}

func TestDSNBuildsFromParts(t *testing.T) {
	clearEnv(t)

	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_PORT", "6432")
	t.Setenv("DB_USER", "spirit")
	t.Setenv("DB_PASSWORD", "secret")
	t.Setenv("DB_NAME", "spiritvpn")
	t.Setenv("DB_SSLMODE", "verify-full")

	u, err := url.Parse(dsn())
	if err != nil {
		t.Fatalf("dsn() выдал неразбираемый URL: %v", err)
	}

	if u.Host != "db.internal:6432" {
		t.Errorf("host %q, ожидался db.internal:6432", u.Host)
	}
	if u.User.Username() != "spirit" {
		t.Errorf("пользователь %q, ожидался spirit", u.User.Username())
	}
	if u.Path != "/spiritvpn" {
		t.Errorf("база %q, ожидалась /spiritvpn", u.Path)
	}
	if got := u.Query().Get("sslmode"); got != "verify-full" {
		t.Errorf("sslmode %q, ожидался verify-full", got)
	}
}

// Пароль из генератора содержит что угодно, и в строке подключения он обязан
// быть экранирован. Незакодированные @ и / разрезают URL по границам host и
// path: драйвер молча получит другой хост и другую базу, а не ошибку разбора.
func TestDSNEscapesPasswordPunctuation(t *testing.T) {
	clearEnv(t)

	const password = "p@ss/w:rd?&#x"
	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_NAME", "spiritvpn")
	t.Setenv("DB_PASSWORD", password)

	raw := dsn()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("dsn() выдал неразбираемый URL: %v", err)
	}

	got, set := u.User.Password()
	if !set {
		t.Fatal("пароль потерялся при сборке строки подключения")
	}
	if got != password {
		t.Errorf("пароль после разбора %q, ожидался %q", got, password)
	}
	// Хост и база обязаны пережить пароль со слэшем и собакой.
	if u.Host != "db.internal:5432" {
		t.Errorf("host %q, ожидался db.internal:5432: пароль разрезал URL", u.Host)
	}
	if u.Path != "/spiritvpn" {
		t.Errorf("база %q, ожидалась /spiritvpn: пароль разрезал URL", u.Path)
	}
	if strings.Contains(raw, password) {
		t.Error("пароль попал в строку подключения неэкранированным")
	}
}

// Пустая переменная равнозначна незаданной: в compose и в systemd пустое значение
// задаётся так же легко, как отсутствующее, и подстановка его вместо умолчания
// даёт заведомо нерабочую строку подключения.
func TestEnvTreatsEmptyAsUnset(t *testing.T) {
	t.Setenv("SPIRIT_TEST_ENV", "")
	if got := env("SPIRIT_TEST_ENV", "по умолчанию"); got != "по умолчанию" {
		t.Errorf("env() = %q, ожидалось умолчание", got)
	}

	t.Setenv("SPIRIT_TEST_ENV", "значение")
	if got := env("SPIRIT_TEST_ENV", "по умолчанию"); got != "значение" {
		t.Errorf("env() = %q, ожидалось значение переменной", got)
	}
}

// migrate.ErrNoChange означает «накатывать нечего», и это штатный исход: deploy.sh
// прогоняет up на каждой выкатке, а миграции появляются не в каждой.
func TestReportTreatsNoChangeAsSuccess(t *testing.T) {
	var err error
	stdout, _ := capture(t, func() { err = report("up", migrate.ErrNoChange) })

	if err != nil {
		t.Fatalf("report вернул %v, ожидался успех", err)
	}
	if !strings.Contains(stdout, "изменений нет") {
		t.Errorf("stdout %q не отличает «нечего делать» от выполненной миграции", stdout)
	}
}

func TestReportSuccessNamesAction(t *testing.T) {
	var err error
	stdout, _ := capture(t, func() { err = report("up", nil) })

	if err != nil {
		t.Fatalf("report вернул %v, ожидался успех", err)
	}
	if !strings.Contains(stdout, "up: ok") {
		t.Errorf("stdout %q, ожидалось подтверждение выполненного действия", stdout)
	}
	if strings.Contains(stdout, "изменений нет") {
		t.Error("выполненная миграция отчиталась как «изменений нет»")
	}
}

// Отказ обязан дойти до вызывающего: main по нему выходит с кодом 1, и deploy.sh
// на этом останавливается, не подменяя образ процесса.
func TestReportWrapsFailure(t *testing.T) {
	broken := errors.New("синтаксическая ошибка в миграции")

	var err error
	capture(t, func() { err = report("down", broken) })

	if err == nil {
		t.Fatal("report скрыл отказ миграции: деплой продолжится на сломанной схеме")
	}
	if !errors.Is(err, broken) {
		t.Errorf("ошибка %v не заворачивает исходную", err)
	}
	if !strings.Contains(err.Error(), "down") {
		t.Errorf("ошибка %q не называет действие", err)
	}
}

// Подсказка обязана перечислять все команды, которые понимает run. Разойдясь с
// ним, она отправит оператора вводить несуществующую команду в момент деплоя.
func TestUsageListsEveryAcceptedCommand(t *testing.T) {
	_, stderr := capture(t, usage)

	for _, cmd := range []string{"up", "down", "down-all", "version", "force"} {
		if !strings.Contains(stderr, cmd) {
			t.Errorf("подсказка не называет команду %q: %q", cmd, stderr)
		}
	}
}
