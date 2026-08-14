package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
	"github.com/RomanRyabinkin/SpiritVPN/internal/grpcsvc"
)

// Тесты остановки процесса. Проверяется утверждение, на которое опирается замена
// контейнера при выкатке: принятая команда доигрывается до конца, и продукт
// получает на неё ответ, а не обрыв соединения.
//
// mTLS здесь не поднимается: shutdown принимает уже собранный *grpc.Server и о
// том, как проверялся вызывающий, ничего не знает. Границу безопасности проверяет
// mtls_test.go.

// blockingApply держит вызов до тех пор, пока тест его не отпустит.
type blockingApply struct {
	entered chan struct{}
	release chan struct{}

	mu   sync.Mutex
	done bool
}

func (a *blockingApply) Execute(context.Context, app.ApplyCustomerCommand) error {
	close(a.entered)
	<-a.release

	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = true
	return nil
}

func (a *blockingApply) completed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done
}

// idleLinks закрывает второй порт сервиса; в этих тестах он не вызывается.
type idleLinks struct{}

func (idleLinks) Execute(context.Context, string) ([]app.CustomerAccessLink, error) {
	return nil, nil
}

// shutdownFixture — обе поверхности на живых слушателях, как в serve.
type shutdownFixture struct {
	grpcServer *grpc.Server
	httpServer *http.Server
	grpcAddr   string
	httpAddr   string
	apply      *blockingApply
	logs       *bytes.Buffer
	logger     *slog.Logger
}

func newShutdownFixture(t *testing.T) *shutdownFixture {
	t.Helper()

	apply := &blockingApply{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	grpcServer := grpc.NewServer()
	customerv1.RegisterCustomerAccessServiceServer(
		grpcServer, grpcsvc.NewCustomerAccessServer(apply, idleLinks{}))

	grpcListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("слушатель gRPC: %v", err)
	}
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("слушатель HTTP: %v", err)
	}

	httpServer := &http.Server{
		Handler:           http.HandlerFunc(handleLive),
		ReadHeaderTimeout: time.Second,
	}

	go func() { _ = grpcServer.Serve(grpcListener) }()
	go func() { _ = httpServer.Serve(httpListener) }()

	// Обе поверхности обязаны уже принимать соединения к моменту возврата.
	//
	// Serve регистрирует слушатель внутри себя, и остановка, обогнавшая эту
	// регистрацию, закрывает слушатель уже в самой горутине — асинхронно.
	// Проверка «после остановки не дозвониться» на такой гонке проходит через
	// раз, причём мимо проверяемого утверждения.
	waitAccepting(t, grpcListener.Addr().String())
	waitAccepting(t, httpListener.Addr().String())

	logs := &bytes.Buffer{}

	return &shutdownFixture{
		grpcServer: grpcServer,
		httpServer: httpServer,
		grpcAddr:   grpcListener.Addr().String(),
		httpAddr:   httpListener.Addr().String(),
		apply:      apply,
		logs:       logs,
		logger:     slog.New(slog.NewTextHandler(logs, nil)),
	}
}

// waitAccepting дожидается, пока по адресу начнут принимать соединения.
func waitAccepting(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("слушатель %s так и не начал принимать соединения: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Незавершённый RPC доигрывается: GracefulStop ждёт обработчик, и вызывающий
// получает ответ. Оборви его здесь — продукт увидел бы UNAVAILABLE на команде,
// которая на самом деле зафиксирована в PostgreSQL.
func TestShutdownWaitsForInFlightRPC(t *testing.T) {
	fixture := newShutdownFixture(t)

	conn, err := grpc.NewClient(fixture.grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	defer func() { _ = conn.Close() }()

	rpcErr := make(chan error, 1)
	go func() {
		_, callErr := customerv1.NewCustomerAccessServiceClient(conn).ApplyCustomerAccess(
			context.Background(), &customerv1.ApplyCustomerAccessRequest{
				CustomerId:        "customer-shutdown",
				VpnFleetId:        42,
				UsageQuotaBytes:   1 << 30,
				ExpiresAtEpochSec: time.Now().Add(time.Hour).Unix(),
				CommandNumber:     1,
			})
		rpcErr <- callErr
	}()

	// Вызов действительно дошёл до обработчика. Без этого тест останавливал бы
	// сервер до начала RPC и ничего бы не проверял.
	select {
	case <-fixture.apply.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("обработчик не был вызван")
	}

	returned := make(chan struct{})
	go func() {
		shutdown(fixture.logger, fixture.grpcServer, fixture.httpServer)
		close(returned)
	}()

	// Пока обработчик держит вызов, shutdown обязан ждать.
	select {
	case <-returned:
		t.Fatal("shutdown вернулся, не дождавшись обработчика: RPC был бы оборван")
	case <-time.After(100 * time.Millisecond):
	}
	if fixture.apply.completed() {
		t.Fatal("обработчик завершился сам: тест не проверяет ожидание")
	}

	close(fixture.apply.release)

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown не завершился после освобождения обработчика")
	}

	select {
	case err := <-rpcErr:
		if err != nil {
			t.Errorf("RPC вернул %v, ожидался ответ: вызов оборвали на остановке", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("клиент не получил ответа")
	}

	// Ушли по ветке ожидания, а не по бюджету: разрыв соединений пишет
	// предупреждение, и его отсутствие отличает одну ветку select от другой.
	if strings.Contains(fixture.logs.String(), "не уложился в бюджет") {
		t.Errorf("остановка пошла по таймауту: %s", fixture.logs.String())
	}
}

// Обе поверхности перестают принимать соединения. HTTP закрывается вместе с
// gRPC: readiness, отвечающий на остановленном процессе, держал бы на нём
// трафик балансировщика.
func TestShutdownClosesBothSurfaces(t *testing.T) {
	fixture := newShutdownFixture(t)
	// Вызовов нет, GracefulStop возвращается сразу.
	close(fixture.apply.release)

	shutdown(fixture.logger, fixture.grpcServer, fixture.httpServer)

	if _, err := net.DialTimeout("tcp", fixture.httpAddr, time.Second); err == nil {
		t.Error("HTTP продолжает принимать соединения после остановки")
	}

	conn, err := grpc.NewClient(fixture.grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = customerv1.NewCustomerAccessServiceClient(conn).ApplyCustomerAccess(
		ctx, &customerv1.ApplyCustomerAccessRequest{CustomerId: "customer-shutdown"})
	if err == nil {
		t.Error("gRPC обслужил вызов после остановки")
	}

	// Повторная остановка уже остановленного сервера безопасна: serve вызывает
	// shutdown и по сигналу, и по отказу поверхности.
	if err := fixture.httpServer.Shutdown(context.Background()); err != nil {
		t.Errorf("повторная остановка HTTP вернула %v", err)
	}
}
