package vpn

import (
	"context"
	"fmt"

	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/vless"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// XrayClient управляет подключением к Xray Core через gRPC
type XrayClient struct {
	client     command.HandlerServiceClient
	connection *grpc.ClientConn
	inboundTag string
}

// NewXrayClient создает новый клиент для Xray API
func NewXrayClient(address string, port int, inboundTag string) (*XrayClient, error) {
	target := fmt.Sprintf("%s:%d", address, port)

	// Подключаемся к gRPC серверу Xray
	// Используем insecure credentials, так как обычно это локальное подключение внутри Docker сети
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to xray api: %w", err)
	}

	client := command.NewHandlerServiceClient(conn)

	return &XrayClient{
		client:     client,
		connection: conn,
		inboundTag: inboundTag,
	}, nil
}

// Close закрывает соединение с Xray
func (c *XrayClient) Close() error {
	return c.connection.Close()
}

// AddUser добавляет пользователя VLESS по UUID
func (c *XrayClient) AddUser(ctx context.Context, uuid string, email string) error {
	// Создаем VLESS аккаунт
	account := &vless.Account{
		Id: uuid,
		// Flow: "xtls-rprx-vision", // Можно добавить если нужно, но для базового VLESS+Reality часто не обязательно в конфиге юзера, если сервер настроен
	}

	// Сериализуем аккаунт в Any
	accountAny := serial.ToTypedMessage(account)

	// Создаем пользователя
	user := &protocol.User{
		Level:   0,
		Email:   email,
		Account: accountAny,
	}

	// Создаем операцию добавления
	op := &command.AddUserOperation{
		User: user,
	}
	opAny := serial.ToTypedMessage(op)

	// Выполняем запрос
	_, err := c.client.AlterInbound(ctx, &command.AlterInboundRequest{
		Tag:       c.inboundTag,
		Operation: opAny,
	})

	if err != nil {
		return fmt.Errorf("failed to add user to xray: %w", err)
	}

	return nil
}

// RemoveUser удаляет пользователя по Email
func (c *XrayClient) RemoveUser(ctx context.Context, email string) error {
	// Создаем операцию удаления
	op := &command.RemoveUserOperation{
		Email: email,
	}
	opAny := serial.ToTypedMessage(op)

	// Выполняем запрос
	_, err := c.client.AlterInbound(ctx, &command.AlterInboundRequest{
		Tag:       c.inboundTag,
		Operation: opAny,
	})

	if err != nil {
		return fmt.Errorf("failed to remove user from xray: %w", err)
	}

	return nil
}

// GetStats получает статистику трафика (заглушка, требует StatsServiceClient)
func (c *XrayClient) GetStats(ctx context.Context, email string) (int64, int64, error) {
	// TODO: Реализовать через stats.StatsServiceClient
	// Нужно будет добавить StatsServiceClient в структуру XrayClient
	return 0, 0, nil
}
