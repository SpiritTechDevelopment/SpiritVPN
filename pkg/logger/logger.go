package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	// Log - глобальный логгер приложения
	Log *logrus.Logger
)

// Setup инициализирует глобальный логгер с заданной конфигурацией.
//
// Parameters:
//   - config: конфигурация логгера
//
// Example:
//
//	logger.Setup(&logger.Config{
//	    Level:         "info",
//	    LogDir:        "./logs",
//	    ConsoleOutput: true,
//	    FileOutput:    true,
//	    ColoredOutput: true,
//	})
func Setup(config *Config) error {
	if !config.Enabled {
		// Отключаем все логирование
		Log = logrus.New()
		Log.SetOutput(io.Discard)
		Log.SetLevel(logrus.PanicLevel)
		return nil
	}

	Log = logrus.New()

	level, err := logrus.ParseLevel(config.Level)
	if err != nil {
		level = logrus.InfoLevel
	}
	Log.SetLevel(level)

	if config.FileOutput && config.LogDir != "" {
		if err := os.MkdirAll(config.LogDir, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}
	}

	var writers []io.Writer

	if config.ConsoleOutput {
		writers = append(writers, os.Stdout)
	}

	if config.FileOutput && config.LogDir != "" {
		mainLogFile := filepath.Join(config.LogDir, "spirit_vpn.log")
		fileWriter := &lumberjack.Logger{
			Filename:   mainLogFile,
			MaxSize:    config.MaxFileSize, // мегабайты
			MaxBackups: config.MaxBackups,
			MaxAge:     config.MaxAge, // дни
			Compress:   true,
		}
		writers = append(writers, fileWriter)

		if config.ErrorLogFile {
			errorLogFile := filepath.Join(config.LogDir, "spirit_vpn_error.log")
			errorWriter := &lumberjack.Logger{
				Filename:   errorLogFile,
				MaxSize:    config.MaxFileSize,
				MaxBackups: config.MaxBackups,
				MaxAge:     config.MaxAge,
				Compress:   true,
			}

			Log.AddHook(&ErrorFileHook{
				Writer:    errorWriter,
				LogLevels: []logrus.Level{logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel},
			})
		}
	}

	if len(writers) > 0 {
		Log.SetOutput(io.MultiWriter(writers...))
	}

	if config.ColoredOutput && config.ConsoleOutput {
		Log.SetFormatter(&ColorFormatter{
			TimestampFormat: config.TimestampFormat,
		})
	} else {
		Log.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: config.TimestampFormat,
			DisableColors:   !config.ColoredOutput,
		})
	}

	// Добавляем Telegram хук для критических ошибок
	if config.TelegramBotToken != "" && config.TelegramChatID != "" {
		telegramHook := NewTelegramHook(config.TelegramBotToken, config.TelegramChatID, config.TelegramThreadID)
		Log.AddHook(telegramHook)
	}

	Log.Infof("Logger initialized: level=%s, dir=%s", config.Level, config.LogDir)
	return nil
}

// GetLogger возвращает логгер с контекстными полями.
// Аналог get_logger() из Python версии.
//
// Parameters:
//   - name: имя логгера (обычно название модуля/пакета)
//   - fields: дополнительные поля для контекста
//
// Example:
//
//	log := logger.GetLogger("vpn.server", logrus.Fields{
//	    "user_id": 123,
//	    "action":  "connect",
//	})
//	log.Info("User connected")
func GetLogger(name string, fields ...logrus.Fields) *logrus.Entry {
	if Log == nil {
		Setup(DefaultConfig())
	}

	entry := Log.WithField("module", name)

	for _, f := range fields {
		entry = entry.WithFields(f)
	}

	return entry
}

// WithContext создает логгер с контекстными полями.
func WithContext(fields logrus.Fields) *logrus.Entry {
	if Log == nil {
		Setup(DefaultConfig())
	}
	return Log.WithFields(fields)
}

// Debug логирует отладочное сообщение
func Debug(args ...interface{}) {
	if Log != nil {
		Log.Debug(args...)
	}
}

// Debugf логирует форматированное отладочное сообщение
func Debugf(format string, args ...interface{}) {
	if Log != nil {
		Log.Debugf(format, args...)
	}
}

// Info логирует информационное сообщение
func Info(args ...interface{}) {
	if Log != nil {
		Log.Info(args...)
	}
}

// Infof логирует форматированное информационное сообщение
func Infof(format string, args ...interface{}) {
	if Log != nil {
		Log.Infof(format, args...)
	}
}

// Warn логирует предупреждение
func Warn(args ...interface{}) {
	if Log != nil {
		Log.Warn(args...)
	}
}

// Warnf логирует форматированное предупреждение
func Warnf(format string, args ...interface{}) {
	if Log != nil {
		Log.Warnf(format, args...)
	}
}

// Error логирует ошибку
func Error(args ...interface{}) {
	if Log != nil {
		Log.Error(args...)
	}
}

// Errorf логирует форматированную ошибку
func Errorf(format string, args ...interface{}) {
	if Log != nil {
		Log.Errorf(format, args...)
	}
}

// Fatal логирует фатальную ошибку и завершает программу
func Fatal(args ...interface{}) {
	if Log != nil {
		Log.Fatal(args...)
	}
}

// Fatalf логирует форматированную фатальную ошибку и завершает программу
func Fatalf(format string, args ...interface{}) {
	if Log != nil {
		Log.Fatalf(format, args...)
	}
}

// Panic логирует панику
func Panic(args ...interface{}) {
	if Log != nil {
		Log.Panic(args...)
	}
}

// Panicf логирует форматированную панику
func Panicf(format string, args ...interface{}) {
	if Log != nil {
		Log.Panicf(format, args...)
	}
}
