package vpn

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/xtls/xray-core/app/proxyman/command"
	statsCommand "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	testUUID       = "550e8400-e29b-41d4-a716-446655440000"
	testEmail      = "test@example.com"
	testInboundTag = "vless-inbound"
)

// Мок для HandlerClient.
type MockHandlerClient struct {
	mock.Mock
}

func (m *MockHandlerClient) AlterInbound(ctx context.Context, in *command.AlterInboundRequest, opts ...grpc.CallOption) (*command.AlterInboundResponse, error) {
	args := m.Called(ctx, in)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*command.AlterInboundResponse), args.Error(1)
}

// Мок для StatsClient.
type MockStatsClient struct {
	mock.Mock
}

func (m *MockStatsClient) QueryStats(ctx context.Context, in *statsCommand.QueryStatsRequest, opts ...grpc.CallOption) (*statsCommand.QueryStatsResponse, error) {
	args := m.Called(ctx, in)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*statsCommand.QueryStatsResponse), args.Error(1)
}

// Проверка что моки реализуют нужные интерфейсы
var _ HandlerClient = (*MockHandlerClient)(nil)
var _ StatsClient = (*MockStatsClient)(nil)

// TestAddUser проверяет добавление пользователя через gRPC API.
// Тестирует корректность формирования запроса AlterInboundRequest с операцией AddUserOperation.
func TestAddUser(t *testing.T) {
	tests := []struct {
		name      string
		uuid      string
		email     string
		mockError error
		wantErr   bool
	}{
		{
			name:      "successful add user",
			uuid:      testUUID,
			email:     testEmail,
			mockError: nil,
			wantErr:   false,
		},
		{
			name:      "grpc error",
			uuid:      testUUID,
			email:     testEmail,
			mockError: errors.New("grpc connection failed"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := new(MockHandlerClient)
			mockStatsClient := new(MockStatsClient)

			xrayClient := &XrayClient{
				client:      mockClient,
				inboundTag:  testInboundTag,
				statsClient: mockStatsClient,
			}

			ctx := context.Background()

			mockClient.On("AlterInbound", ctx, mock.MatchedBy(func(req *command.AlterInboundRequest) bool {
				return req.Tag == testInboundTag && req.Operation != nil
			})).Return(&command.AlterInboundResponse{}, tt.mockError)

			err := xrayClient.AddUser(ctx, tt.uuid, tt.email)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "failed to add user to xray")
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
		})
	}
}

// TestRemoveUser проверяет удаление пользователя через gRPC API.
// Тестирует корректность формирования запроса AlterInboundRequest с операцией RemoveUserOperation.
func TestRemoveUser(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		mockError error
		wantErr   bool
	}{
		{
			name:      "successful remove user",
			email:     testEmail,
			mockError: nil,
			wantErr:   false,
		},
		{
			name:      "grpc error",
			email:     testEmail,
			mockError: errors.New("user not found"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := new(MockHandlerClient)
			mockStatsClient := new(MockStatsClient)

			xrayClient := &XrayClient{
				client:      mockClient,
				inboundTag:  testInboundTag,
				statsClient: mockStatsClient,
			}

			ctx := context.Background()

			mockClient.On("AlterInbound", ctx, mock.MatchedBy(func(req *command.AlterInboundRequest) bool {
				return req.Tag == testInboundTag && req.Operation != nil
			})).Return(&command.AlterInboundResponse{}, tt.mockError)

			err := xrayClient.RemoveUser(ctx, tt.email)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "failed to remove user from xray")
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
		})
	}
}

// TestGetStats проверяет получение статистики трафика пользователя.
// Тестирует корректность парсинга ответа QueryStatsResponse и обработку ошибок.
func TestGetStats(t *testing.T) {
	tests := []struct {
		name           string
		email          string
		mockResponse   *statsCommand.QueryStatsResponse
		mockError      error
		expectedDown   int64
		expectedUp     int64
		wantErr        bool
		statsClientNil bool
	}{
		{
			name:  "successful get stats",
			email: testEmail,
			mockResponse: &statsCommand.QueryStatsResponse{
				Stat: []*statsCommand.Stat{
					{Name: "user>>>" + testEmail + ">>>traffic>>>uplink", Value: 1024},
					{Name: "user>>>" + testEmail + ">>>traffic>>>downlink", Value: 2048},
				},
			},
			mockError:    nil,
			expectedDown: 2048,
			expectedUp:   1024,
			wantErr:      false,
		},
		{
			name:         "grpc error",
			email:        testEmail,
			mockResponse: nil,
			mockError:    errors.New("connection timeout"),
			wantErr:      true,
		},
		{
			name:           "stats client not initialized",
			email:          testEmail,
			statsClientNil: true,
			wantErr:        true,
		},
		{
			name:  "empty stats response",
			email: testEmail,
			mockResponse: &statsCommand.QueryStatsResponse{
				Stat: []*statsCommand.Stat{},
			},
			mockError:    nil,
			expectedDown: 0,
			expectedUp:   0,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := new(MockHandlerClient)
			mockStatsClient := new(MockStatsClient)

			xrayClient := &XrayClient{
				client:      mockClient,
				inboundTag:  testInboundTag,
				statsClient: mockStatsClient,
			}

			if tt.statsClientNil {
				xrayClient.statsClient = nil
			}

			ctx := context.Background()

			if !tt.statsClientNil {
				mockStatsClient.On("QueryStats", ctx, mock.MatchedBy(func(req *statsCommand.QueryStatsRequest) bool {
					return req.Pattern == "user>>>"+tt.email+">>>traffic>>>" && req.Reset_ == false
				})).Return(tt.mockResponse, tt.mockError)
			}

			downlink, uplink, err := xrayClient.GetStats(ctx, tt.email)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedDown, downlink)
				assert.Equal(t, tt.expectedUp, uplink)
			}

			if !tt.statsClientNil {
				mockStatsClient.AssertExpectations(t)
			}
		})
	}
}

// TestAddUserValidation проверяет валидацию входных данных при добавлении пользователя.
func TestAddUserValidation(t *testing.T) {
	mockClient := new(MockHandlerClient)
	mockStatsClient := new(MockStatsClient)

	xrayClient := &XrayClient{
		client:      mockClient,
		inboundTag:  testInboundTag,
		statsClient: mockStatsClient,
	}

	ctx := context.Background()

	// Проверка что метод вызывается с корректными параметрами
	mockClient.On("AlterInbound", ctx, mock.Anything).Return(&command.AlterInboundResponse{}, nil)

	err := xrayClient.AddUser(ctx, testUUID, testEmail)
	assert.NoError(t, err)

	// Проверка что AlterInbound был вызван ровно один раз
	mockClient.AssertNumberOfCalls(t, "AlterInbound", 1)
}

// TestRemoveUserValidation проверяет валидацию входных данных при удалении пользователя.
func TestRemoveUserValidation(t *testing.T) {
	mockClient := new(MockHandlerClient)
	mockStatsClient := new(MockStatsClient)

	xrayClient := &XrayClient{
		client:      mockClient,
		inboundTag:  testInboundTag,
		statsClient: mockStatsClient,
	}

	ctx := context.Background()

	// Проверяем что метод вызывается с корректными параметрами
	mockClient.On("AlterInbound", ctx, mock.Anything).Return(&command.AlterInboundResponse{}, nil)

	err := xrayClient.RemoveUser(ctx, testEmail)
	assert.NoError(t, err)

	// Проверяем что AlterInbound был вызван ровно один раз
	mockClient.AssertNumberOfCalls(t, "AlterInbound", 1)
}

// TestClose проверяет корректное закрытие соединения.
func TestClose(t *testing.T) {
	conn, err := grpc.NewClient("localhost:0", grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)

	xrayClient := &XrayClient{
		connection: conn,
	}

	err = xrayClient.Close()
	assert.NoError(t, err)
}
