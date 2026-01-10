package logger

import (
	"io"

	"github.com/sirupsen/logrus"
)

// ErrorFileHook - хук для записи ошибок в отдельный файл.
// Используется для создания отдельного error.log файла.
type ErrorFileHook struct {
	Writer    io.Writer
	LogLevels []logrus.Level
}

// Levels возвращает уровни логирования, для которых срабатывает хук
func (hook *ErrorFileHook) Levels() []logrus.Level {
	return hook.LogLevels
}

// Fire вызывается при записи лога
func (hook *ErrorFileHook) Fire(entry *logrus.Entry) error {
	line, err := entry.String()
	if err != nil {
		return err
	}
	_, err = hook.Writer.Write([]byte(line))
	return err
}
