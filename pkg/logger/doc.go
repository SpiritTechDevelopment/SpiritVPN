// Package logger предоставляет структурированное логирование для SpiritVPN.
//
// Основные возможности:
//   - Цветной вывод в консоль
//   - Ротация лог-файлов
//   - Разные уровни логирования (Debug, Info, Warn, Error, Fatal, Panic)
//   - Отдельный файл для ошибок
//   - Отправка критических ошибок в Telegram
//   - Контекстные поля для структурированного логирования
//
// Пример использования:
//
//	import (
//	    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
//	    "github.com/sirupsen/logrus"
//	)
//
//	func main() {
//	    // Настройка логгера
//	    config := &logger.Config{
//	        Level:         "info",
//	        LogDir:        "./logs",
//	        ConsoleOutput: true,
//	        FileOutput:    true,
//	        ColoredOutput: true,
//	    }
//	    logger.Setup(config)
//
//	    // Простое логирование
//	    logger.Info("Application started")
//	    logger.Errorf("Failed to connect: %v", err)
//
//	    // Логирование с контекстом
//	    log := logger.GetLogger("vpn.server", logrus.Fields{
//	        "user_id": 123,
//	        "action":  "connect",
//	    })
//	    log.Info("User connecting to VPN")
//
//	    // Специализированные контексты
//	    userLog := logger.WithUserContext(123)
//	    userLog.Info("User action performed")
//	}
package logger
