package logger

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
)

// ANSI цветовые коды для форматирования вывода в терминале.
// Используются для цветного отображения различных уровней логирования.
const (
	ColorReset   = "\033[0m"  // Сброс всех атрибутов форматирования
	ColorRed     = "\033[31m" // Красный цвет (используется для ERROR)
	ColorGreen   = "\033[32m" // Зеленый цвет (используется для INFO)
	ColorYellow  = "\033[33m" // Желтый цвет (используется для WARNING)
	ColorBlue    = "\033[34m" // Синий цвет
	ColorMagenta = "\033[35m" // Пурпурный цвет (используется для FATAL/PANIC)
	ColorCyan    = "\033[36m" // Голубой цвет (используется для DEBUG)
	ColorWhite   = "\033[37m" // Белый цвет
	ColorGray    = "\033[90m" // Серый цвет (используется для метаданных)
)

// ColorFormatter - форматтер логов с цветным выводом для консоли.
// Реализует интерфейс logrus.Formatter.
//
// Форматтер автоматически раскрашивает логи в зависимости от уровня логирования,
// добавляет информацию о вызывающем коде (файл:строка:функция) и форматирует
// структурированные поля.
//
// Формат вывода:
//
//	[TIMESTAMP] [LEVEL] [CALLER] MESSAGE {fields}
//
// где:
//   - TIMESTAMP - временная метка в формате TimestampFormat (серый цвет)
//   - LEVEL - уровень логирования с соответствующим цветом
//   - CALLER - информация о месте вызова: файл:строка:функция (голубой)
//   - MESSAGE - текст сообщения
//   - fields - дополнительные структурированные поля (серый)
//
// Пример использования:
//
//	formatter := &ColorFormatter{
//	    TimestampFormat: time.RFC3339,
//	}
//	log.SetFormatter(formatter)
type ColorFormatter struct {
	// TimestampFormat определяет формат временной метки в логах.
	// Использует стандартные форматы Go (например, time.RFC3339).
	// Если не установлен, используется формат по умолчанию.
	TimestampFormat string
}

// Format форматирует запись лога в байтовый срез с применением цветов.
// Реализует интерфейс logrus.Formatter.
//
// Возвращает форматированную строку в виде []byte и ошибку (всегда nil).
func (f *ColorFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	var b bytes.Buffer

	levelColor := f.getLevelColor(entry.Level)
	levelText := strings.ToUpper(entry.Level.String())

	timestamp := entry.Time.Format(f.TimestampFormat)

	caller := f.getCaller()

	fmt.Fprintf(&b, "%s[%s]%s ", ColorGray, timestamp, ColorReset)
	fmt.Fprintf(&b, "%s[%-7s]%s ", levelColor, levelText, ColorReset)

	if caller != "" {
		fmt.Fprintf(&b, "%s[%s]%s ", ColorCyan, caller, ColorReset)
	}

	fmt.Fprintf(&b, "%s", entry.Message)

	if len(entry.Data) > 0 {
		fmt.Fprintf(&b, " %s%s%s", ColorGray, f.formatFields(entry.Data), ColorReset)
	}

	b.WriteByte('\n')
	return b.Bytes(), nil
}

// getLevelColor возвращает ANSI код цвета для указанного уровня логирования.
//
// Соответствие уровней и цветов:
//   - DEBUG -> Cyan (голубой)
//   - INFO -> Green (зеленый)
//   - WARNING -> Yellow (желтый)
//   - ERROR -> Red (красный)
//   - FATAL/PANIC -> Magenta (пурпурный)
//
// Параметры:
//   - level: уровень логирования из logrus.Level
//
// Возвращает:
//   - строку с ANSI кодом цвета
func (f *ColorFormatter) getLevelColor(level logrus.Level) string {
	switch level {
	case logrus.DebugLevel:
		return ColorCyan
	case logrus.InfoLevel:
		return ColorGreen
	case logrus.WarnLevel:
		return ColorYellow
	case logrus.ErrorLevel:
		return ColorRed
	case logrus.FatalLevel, logrus.PanicLevel:
		return ColorMagenta
	default:
		return ColorWhite
	}
}

// getCaller извлекает информацию о месте вызова логгера из стека вызовов.
//
// Функция анализирует стек вызовов, пропуская фреймы самого логгера и logrus,
// чтобы найти реальное место вызова в пользовательском коде.
//
// Формат возвращаемой строки: "файл:строка:функция"
// Например: "server.go:42:Start"
//
// Возвращает:
//   - строку с информацией о вызывающем коде
//   - пустую строку, если не удалось определить место вызова
func (f *ColorFormatter) getCaller() string {
	for i := 3; i < 10; i++ {
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}

		// Получаем имя функции
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			continue
		}

		fnName := fn.Name()

		if strings.Contains(fnName, "logrus") ||
			strings.Contains(fnName, "logger.") ||
			strings.Contains(file, "logrus") {
			continue
		}

		shortFile := filepath.Base(file)

		shortFnName := fnName
		if lastSlash := strings.LastIndex(fnName, "/"); lastSlash >= 0 {
			shortFnName = fnName[lastSlash+1:]
		}
		if lastDot := strings.LastIndex(shortFnName, "."); lastDot >= 0 {
			shortFnName = shortFnName[lastDot+1:]
		}

		return fmt.Sprintf("%s:%d:%s", shortFile, line, shortFnName)
	}

	return ""
}

// formatFields форматирует структурированные поля логируемой записи в строку.
//
// Преобразует map полей в строку формата "{key1=value1, key2=value2}".
// Используется для отображения контекстной информации в логах.
//
// Параметры:
//   - fields: карта полей из logrus.Fields
//
// Возвращает:
//   - строку с отформатированными полями в фигурных скобках
//
// Пример:
//
//	fields := logrus.Fields{"user_id": 123, "action": "login"}
//	result := formatFields(fields)
//	// result: "{user_id=123, action=login}"
func (f *ColorFormatter) formatFields(fields logrus.Fields) string {
	var parts []string
	for key, value := range fields {
		parts = append(parts, fmt.Sprintf("%s=%v", key, value))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
