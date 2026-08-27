package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/config"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
	manifestv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/manifest/v1"
	"github.com/RomanRyabinkin/SpiritVPN/internal/grpcsvc"
)

// applyUseCase — то, что транспорту нужно от use case.
//
// Интерфейс, а не *app.ApplyCustomerAccess: интеграционный тест mTLS поднимает
// настоящий сервер с настоящим рукопожатием, и пул PostgreSQL ему для проверки
// границы безопасности не нужен.
type applyUseCase interface {
	Execute(ctx context.Context, cmd app.ApplyCustomerCommand) error
}

// linksUseCase — то же самое для read-пути.
type linksUseCase interface {
	Execute(ctx context.Context, customerID string) ([]app.CustomerAccessLink, error)
}

// availableNodesUseCase — публичный read-путь актуального manifest.
type availableNodesUseCase interface {
	Execute(ctx context.Context) ([]app.AvailableFleet, error)
}

// manifestUseCase — приём infrastructure manifest.
type manifestUseCase interface {
	Execute(ctx context.Context, cmd app.ApplyManifestCommand) (app.ApplyManifestResult, error)
}

// newGRPCServer собирает внешнюю поверхность поверх mTLS.
func newGRPCServer(
	cfg config.GRPC,
	logger *slog.Logger,
	apply applyUseCase,
	links linksUseCase,
	nodes availableNodesUseCase,
	manifest manifestUseCase,
	administration ...grpcsvc.CustomerAccessAdministration,
) (*grpc.Server, error) {
	creds, err := transportCredentials(cfg)
	if err != nil {
		return nil, err
	}

	authorizer := grpcsvc.NewAuthorizer(map[grpcsvc.Role][]string{
		grpcsvc.RoleCustomerAccessWriter: cfg.CustomerAccessWriters,
		grpcsvc.RoleCustomerAccessReader: cfg.CustomerAccessReaders,
		grpcsvc.RoleCustomerAccessAdmin:  cfg.CustomerAccessAdmins,
		grpcsvc.RoleManifestWriter:       cfg.ManifestWriters,
	})

	// Порядок цепочки существенен и держится здесь:
	//
	//  1. request_id первым — иначе логировать будет нечем, и корреляция
	//     потеряется ровно на тех запросах, которые интереснее всего;
	//  2. логирование до авторизации — чтобы отказ авторизации тоже попал в лог.
	//     «Кому именно отказали» — самое ценное в такой записи, а после auth
	//     обработчик уже не вызовется и записи не будет;
	//  3. авторизация последней — дальше идёт хендлер, и до него доходят только
	//     вызовы с подтверждённым правом.
	server := grpc.NewServer(
		grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(
			grpcsvc.RequestIDUnaryInterceptor(uuid.NewString),
			grpcsvc.LoggingUnaryInterceptor(logger),
			authorizer.UnaryInterceptor(),
		),
	)

	customerv1.RegisterCustomerAccessServiceServer(server, grpcsvc.NewCustomerAccessServer(apply, links, nodes, administration...))
	manifestv1.RegisterManifestServiceServer(server, grpcsvc.NewManifestServer(manifest))

	return server, nil
}

// transportCredentials собирает mTLS.
//
// Insecure-режима нет. На втором пути исполнения, где авторизация не
// выполняется, шла бы вся локальная разработка, и ошибки в сопоставлении
// идентичностей и ролей всплывали бы только в production. Локально те же самые
// сертификаты выпускает `make dev-certs`.
func transportCredentials(cfg config.GRPC) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("серверная пара TLS: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("CA клиентов: %w", err)
	}

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		// AppendCertsFromPEM молча игнорирует мусор и возвращает false. Без этой
		// проверки процесс поднялся бы с пустым пулом и отвергал бы всех подряд с
		// диагностикой «недостаточно прав» вместо «CA не прочитан».
		return nil, fmt.Errorf("CA клиентов: %s не содержит ни одного сертификата", cfg.ClientCAFile)
	}

	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},

		// RequireAndVerifyClientCert, и только он. RequireAnyClientCert требует
		// сертификат, но не проверяет цепочку — подойдёт любой самоподписанный;
		// VerifyClientCertIfGiven проверяет лишь предъявленный, то есть не
		// предъявить выгоднее.
		ClientCAs:  clientCAs,
		ClientAuth: tls.RequireAndVerifyClientCert,

		// В TLS 1.3 сертификат клиента передаётся уже зашифрованным; в 1.2 он
		// идёт открытым текстом, и пассивный наблюдатель видит, кто с кем
		// разговаривает.
		MinVersion: tls.VersionTLS13,
	}), nil
}
