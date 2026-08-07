package app

import "time"

// SystemClock — часы процесса поверх time.Now.
//
// Ограничения применимости описаны у порта Clock: это НЕ источник времени для
// доменных решений.
type SystemClock struct{}

// Now возвращает текущее время процесса в UTC. UTC, а не локальная зона, потому
// что весь внешний контракт и вся схема оперируют timestamptz в UTC (§5, §11).
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}
