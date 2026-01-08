package vpn

import (
	"context"
	"fmt"
	"strings"

	"github.com/xtls/xray-core/app/proxyman/command"
	proxymanCommand "github.com/xtls/xray-core/app/proxyman/command"
	statsCommand "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/vless"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// XrayClient управляет подключением к Xray Core через gRPC API.
// Позволяет динамически добавлять/удалять пользователей VLESS и получать статистику трафика.
//
// Fields:
//   - client: HandlerService клиент для управления пользователями (добавление/удаление)
//   - statsClient: StatsService клиент для получения статистики трафика
//   - connection: активное gRPC соединение с Xray API
//   - inboundTag: тег входящего соединения (inbound) в конфиге Xray (обычно "vless-inbound")
type XrayClient struct {
	client      proxymanCommand.HandlerServiceClient
	connection  *grpc.ClientConn
	inboundTag  string
	statsClient statsCommand.StatsServiceClient
}

// NewXrayClient создает и инициализирует новый клиент для подключения к Xray API.
// Устанавливает gRPC соединение с Xray и создает клиенты для HandlerService и StatsService.
//
// Parameters:
//   - address: адрес Xray API сервера (например, "127.0.0.1" или "localhost")
//   - port: порт Xray API (обычно 10085, настраивается в xray.json через dokodemo-door)
//   - inboundTag: тег входящего соединения из конфига Xray (обычно "vless-inbound")
//
// Returns:
//   - *XrayClient: инициализированный клиент для работы с Xray
//   - error: ошибка подключения или nil при успехе
//
// Example:
//
//	client, err := NewXrayClient("127.0.0.1", 10085, "vless-inbound")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
func NewXrayClient(address string, port int, inboundTag string) (*XrayClient, error) {
	target := fmt.Sprintf("%s:%d", address, port)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to xray api: %w", err)
	}

	client := proxymanCommand.NewHandlerServiceClient(conn)
	statsClient := statsCommand.NewStatsServiceClient(conn)

	return &XrayClient{
		client:      client,
		connection:  conn,
		inboundTag:  inboundTag,
		statsClient: statsClient,
	}, nil
}

// Close корректно закрывает gRPC соединение с Xray API.
// Вызывается после завершения работы с клиентом для освобождения ресурсов.
//
// Returns:
//   - error: ошибка закрытия соединения или nil при успехе
func (c *XrayClient) Close() error {
	return c.connection.Close()
}

// AddUser динамически добавляет нового пользователя VLESS в работающий Xray сервер.
//
// Parameters:
//   - ctx: контекст для отмены операции и таймаутов
//   - uuid: уникальный UUID пользователя в формате RFC 4122 (например, "550e8400-e29b-41d4-a716-446655440000")
//   - email: идентификатор пользователя (используется для статистики и удаления, обычно совпадает с UUID)
//
// Returns:
//   - error: ошибка добавления пользователя или nil при успехе
//
// Notes:
//   - UUID должен быть валидным UUIDv4
//   - Email используется как ключ для получения статистики и удаления пользователя
//   - Изменения применяются без перезапуска Xray
func (c *XrayClient) AddUser(ctx context.Context, uuid string, email string) error {
	account := &vless.Account{
		Id: uuid,
	}

	accountAny := serial.ToTypedMessage(account)

	user := &protocol.User{
		Level:   0,
		Email:   email,
		Account: accountAny,
	}

	op := &command.AddUserOperation{
		User: user,
	}
	opAny := serial.ToTypedMessage(op)

	_, err := c.client.AlterInbound(ctx, &command.AlterInboundRequest{
		Tag:       c.inboundTag,
		Operation: opAny,
	})

	if err != nil {
		return fmt.Errorf("failed to add user to xray: %w", err)
	}

	return nil
}

// RemoveUser динамически удаляет пользователя из работающего Xray сервера.
// Пользователь будет немедленно отключен и больше не сможет подключиться к VPN.
//
// Parameters:
//   - ctx: контекст для отмены операции и таймаутов
//   - email: идентификатор пользователя (тот же, что был указан в AddUser)
//
// Returns:
//   - error: ошибка удаления пользователя или nil при успехе
//
// Notes:
//   - Email должен совпадать с тем, что был указан при добавлении пользователя
//   - Активные соединения пользователя будут разорваны
//   - Изменения применяются без перезапуска Xray
func (c *XrayClient) RemoveUser(ctx context.Context, email string) error {
	op := &command.RemoveUserOperation{
		Email: email,
	}
	opAny := serial.ToTypedMessage(op)

	_, err := c.client.AlterInbound(ctx, &command.AlterInboundRequest{
		Tag:       c.inboundTag,
		Operation: opAny,
	})

	if err != nil {
		return fmt.Errorf("failed to remove user from xray: %w", err)
	}

	return nil
}

// GetStats получает статистику использования трафика для конкретного пользователя.
// Возвращает объем загруженных и отправленных данных в байтах.
//
// Parameters:
//   - ctx: контекст для отмены операции и таймаутов
//   - email: идентификатор пользователя (тот же, что был указан в AddUser)
//
// Returns:
//   - int64: количество полученных байт (downlink) с момента добавления пользователя или последнего сброса
//   - int64: количество отправленных байт (uplink) с момента добавления пользователя или последнего сброса
//   - error: ошибка получения статистики или nil при успехе
//
// Notes:
//   - Статистика собирается автоматически, если в конфиге Xray включен StatsService
//   - Счетчики НЕ сбрасываются после вызова (используется Reset_: false)
//   - Если пользователь не найден, возвращает (0, 0, nil)
//
// Example:
//
//	received, sent, err := client.GetStats(ctx, "user@example.com")
//	if err != nil {
//	    log.Printf("Failed to get stats: %v", err)
//	}
//	log.Printf("User traffic: received=%d bytes, sent=%d bytes", received, sent)
func (c *XrayClient) GetStats(ctx context.Context, email string) (int64, int64, error) {
	if c.statsClient == nil {
		return 0, 0, fmt.Errorf("stats client not initialized")
	}

	pattern := "user>>>" + email + ">>>traffic>>>"

	resp, err := c.statsClient.QueryStats(ctx, &statsCommand.QueryStatsRequest{Pattern: pattern, Reset_: false})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to query stats: %w", err)
	}

	var uplink int64
	var downlink int64
	for _, s := range resp.GetStat() {
		if s == nil {
			continue
		}
		name := s.GetName()
		value := s.GetValue()
		if strings.HasSuffix(name, ">>>uplink") {
			uplink += value
		} else if strings.HasSuffix(name, ">>>downlink") {
			downlink += value
		}
	}

	return downlink, uplink, nil
}
