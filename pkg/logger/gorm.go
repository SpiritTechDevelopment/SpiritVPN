package logger

import (
	"context"
	"errors"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// GormLogger адаптер для интеграции логгера с GORM.
// Позволяет логировать SQL запросы через наш логгер.
type GormLogger struct {
	log                   *logrus.Entry
	SlowThreshold         time.Duration
	SkipErrRecordNotFound bool
	LogLevel              gormlogger.LogLevel
}

// NewGormLogger создает новый GORM logger адаптер.
//
// Пример:
//
//	gormLogger := logger.NewGormLogger("database", 200*time.Millisecond)
//	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
//	    Logger: gormLogger,
//	})
func NewGormLogger(moduleName string, slowThreshold time.Duration) gormlogger.Interface {
	return &GormLogger{
		log:                   GetLogger(moduleName),
		SlowThreshold:         slowThreshold,
		SkipErrRecordNotFound: true,
		LogLevel:              gormlogger.Info,
	}
}

// LogMode устанавливает уровень логирования
func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.LogLevel = level
	return &newLogger
}

// Info логирует информационное сообщение
func (l *GormLogger) Info(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= gormlogger.Info {
		l.log.Infof(msg, data...)
	}
}

// Warn логирует предупреждение
func (l *GormLogger) Warn(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= gormlogger.Warn {
		l.log.Warnf(msg, data...)
	}
}

// Error логирует ошибку
func (l *GormLogger) Error(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= gormlogger.Error {
		l.log.Errorf(msg, data...)
	}
}

// Trace логирует выполнение SQL запроса
func (l *GormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.LogLevel <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()

	fields := logrus.Fields{
		"elapsed_ms": elapsed.Milliseconds(),
		"rows":       rows,
	}

	switch {
	case err != nil && l.LogLevel >= gormlogger.Error && (!errors.Is(err, gorm.ErrRecordNotFound) || !l.SkipErrRecordNotFound):
		// Ошибка выполнения запроса
		l.log.WithFields(fields).WithField("sql", sql).WithError(err).Error("SQL query failed")

	case elapsed > l.SlowThreshold && l.SlowThreshold != 0 && l.LogLevel >= gormlogger.Warn:
		// Медленный запрос
		l.log.WithFields(fields).WithField("sql", sql).Warnf("Slow SQL query (threshold: %v)", l.SlowThreshold)

	case l.LogLevel >= gormlogger.Info:
		// Обычный запрос
		l.log.WithFields(fields).Debugf("SQL: %s", sql)
	}
}
