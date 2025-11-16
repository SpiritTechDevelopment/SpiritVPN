package database

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// UserRepository предоставляет методы для работы с пользователями
type UserRepository struct {
	db *gorm.DB
}

// NewUserRepository создает новый репозиторий пользователей
func NewUserRepository(db *DB) *UserRepository {
	return &UserRepository{db: db.GetDB()}
}

// Create создает нового пользователя
func (r *UserRepository) Create(user *User) error {
	return r.db.Create(user).Error
}

// GetByID находит пользователя по ID
func (r *UserRepository) GetByID(id uint) (*User, error) {
	var user User
	err := r.db.First(&user, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// GetByTelegramID находит пользователя по Telegram ID
func (r *UserRepository) GetByTelegramID(telegramID int64) (*User, error) {
	var user User
	err := r.db.Where("telegram_id = ?", telegramID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// Update обновляет данные пользователя
func (r *UserRepository) Update(user *User) error {
	return r.db.Save(user).Error
}

// Delete удаляет пользователя
func (r *UserRepository) Delete(id uint) error {
	return r.db.Delete(&User{}, id).Error
}

// GetWithSubscriptions возвращает пользователя с его подписками
func (r *UserRepository) GetWithSubscriptions(id uint) (*User, error) {
	var user User
	err := r.db.Preload("Subscriptions").First(&user, id).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// SubscriptionRepository предоставляет методы для работы с подписками
type SubscriptionRepository struct {
	db *gorm.DB
}

// NewSubscriptionRepository создает новый репозиторий подписок
func NewSubscriptionRepository(db *DB) *SubscriptionRepository {
	return &SubscriptionRepository{db: db.GetDB()}
}

// Create создает новую подписку
func (r *SubscriptionRepository) Create(subscription *Subscription) error {
	return r.db.Create(subscription).Error
}

// GetByID находит подписку по ID
func (r *SubscriptionRepository) GetByID(id uint) (*Subscription, error) {
	var subscription Subscription
	err := r.db.First(&subscription, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &subscription, nil
}

// GetActiveByUserID возвращает активную подписку пользователя
func (r *SubscriptionRepository) GetActiveByUserID(userID uint) (*Subscription, error) {
	var subscription Subscription
	err := r.db.Where("user_id = ? AND is_active = true AND end_date > ?", userID, time.Now()).
		Order("end_date DESC").
		First(&subscription).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &subscription, nil
}

// GetByUserID возвращает все подписки пользователя
func (r *SubscriptionRepository) GetByUserID(userID uint) ([]Subscription, error) {
	var subscriptions []Subscription
	err := r.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&subscriptions).Error
	return subscriptions, err
}

// Update обновляет подписку
func (r *SubscriptionRepository) Update(subscription *Subscription) error {
	return r.db.Save(subscription).Error
}

// Deactivate деактивирует подписку
func (r *SubscriptionRepository) Deactivate(id uint) error {
	return r.db.Model(&Subscription{}).Where("id = ?", id).Update("is_active", false).Error
}

// GetExpiring возвращает подписки, истекающие в ближайшие N дней
func (r *SubscriptionRepository) GetExpiring(days int) ([]Subscription, error) {
	var subscriptions []Subscription
	endDate := time.Now().AddDate(0, 0, days)
	err := r.db.Where("is_active = true AND end_date BETWEEN ? AND ?", time.Now(), endDate).
		Preload("User").
		Find(&subscriptions).Error
	return subscriptions, err
}

// VPNConfigRepository предоставляет методы для работы с VPN конфигурациями
type VPNConfigRepository struct {
	db *gorm.DB
}

// NewVPNConfigRepository создает новый репозиторий VPN конфигураций
func NewVPNConfigRepository(db *DB) *VPNConfigRepository {
	return &VPNConfigRepository{db: db.GetDB()}
}

// Create создает новую VPN конфигурацию
func (r *VPNConfigRepository) Create(config *VPNConfig) error {
	return r.db.Create(config).Error
}

// GetByID находит конфигурацию по ID
func (r *VPNConfigRepository) GetByID(id uint) (*VPNConfig, error) {
	var config VPNConfig
	err := r.db.Preload("Server").First(&config, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &config, nil
}

// GetByUserID возвращает все конфигурации пользователя
func (r *VPNConfigRepository) GetByUserID(userID uint) ([]VPNConfig, error) {
	var configs []VPNConfig
	err := r.db.Where("user_id = ?", userID).Preload("Server").Find(&configs).Error
	return configs, err
}

// GetByPublicKey находит конфигурацию по публичному ключу
func (r *VPNConfigRepository) GetByPublicKey(publicKey string) (*VPNConfig, error) {
	var config VPNConfig
	err := r.db.Where("public_key = ?", publicKey).First(&config).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &config, nil
}

// Update обновляет конфигурацию
func (r *VPNConfigRepository) Update(config *VPNConfig) error {
	return r.db.Save(config).Error
}

// Delete удаляет конфигурацию
func (r *VPNConfigRepository) Delete(id uint) error {
	return r.db.Delete(&VPNConfig{}, id).Error
}

// VPNServerRepository предоставляет методы для работы с VPN серверами
type VPNServerRepository struct {
	db *gorm.DB
}

// NewVPNServerRepository создает новый репозиторий VPN серверов
func NewVPNServerRepository(db *DB) *VPNServerRepository {
	return &VPNServerRepository{db: db.GetDB()}
}

// Create создает новый сервер
func (r *VPNServerRepository) Create(server *VPNServer) error {
	return r.db.Create(server).Error
}

// GetByID находит сервер по ID
func (r *VPNServerRepository) GetByID(id uint) (*VPNServer, error) {
	var server VPNServer
	err := r.db.First(&server, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &server, nil
}

// GetAll возвращает все серверы
func (r *VPNServerRepository) GetAll() ([]VPNServer, error) {
	var servers []VPNServer
	err := r.db.Order("location").Find(&servers).Error
	return servers, err
}

// GetActive возвращает активные серверы с доступной емкостью
func (r *VPNServerRepository) GetActive() ([]VPNServer, error) {
	var servers []VPNServer
	err := r.db.Where("is_active = true AND current_users < max_users").
		Order("load_percent ASC").
		Find(&servers).Error
	return servers, err
}

// GetOptimal возвращает оптимальный сервер с наименьшей загрузкой
func (r *VPNServerRepository) GetOptimal() (*VPNServer, error) {
	var server VPNServer
	err := r.db.Where("is_active = true AND current_users < max_users").
		Order("load_percent ASC").
		First(&server).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &server, nil
}

// Update обновляет сервер
func (r *VPNServerRepository) Update(server *VPNServer) error {
	return r.db.Save(server).Error
}

// IncrementUsers увеличивает счетчик пользователей на сервере
func (r *VPNServerRepository) IncrementUsers(id uint) error {
	return r.db.Model(&VPNServer{}).Where("id = ?", id).
		UpdateColumn("current_users", gorm.Expr("current_users + 1")).Error
}

// DecrementUsers уменьшает счетчик пользователей на сервере
func (r *VPNServerRepository) DecrementUsers(id uint) error {
	return r.db.Model(&VPNServer{}).Where("id = ?", id).
		Where("current_users > 0").
		UpdateColumn("current_users", gorm.Expr("current_users - 1")).Error
}

// PaymentRepository предоставляет методы для работы с платежами
type PaymentRepository struct {
	db *gorm.DB
}

// NewPaymentRepository создает новый репозиторий платежей
func NewPaymentRepository(db *DB) *PaymentRepository {
	return &PaymentRepository{db: db.GetDB()}
}

// Create создает новый платеж
func (r *PaymentRepository) Create(payment *Payment) error {
	return r.db.Create(payment).Error
}

// GetByID находит платеж по ID
func (r *PaymentRepository) GetByID(id uint) (*Payment, error) {
	var payment Payment
	err := r.db.First(&payment, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &payment, nil
}

// GetByTransactionID находит платеж по ID транзакции
func (r *PaymentRepository) GetByTransactionID(transactionID string) (*Payment, error) {
	var payment Payment
	err := r.db.Where("transaction_id = ?", transactionID).First(&payment).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &payment, nil
}

// GetByUserID возвращает все платежи пользователя
func (r *PaymentRepository) GetByUserID(userID uint) ([]Payment, error) {
	var payments []Payment
	err := r.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&payments).Error
	return payments, err
}

// Update обновляет платеж
func (r *PaymentRepository) Update(payment *Payment) error {
	return r.db.Save(payment).Error
}

// UpdateStatus обновляет статус платежа
func (r *PaymentRepository) UpdateStatus(id uint, status string) error {
	return r.db.Model(&Payment{}).Where("id = ?", id).Update("status", status).Error
}

// SubscriptionPlanRepository предоставляет методы для работы с тарифными планами
type SubscriptionPlanRepository struct {
	db *gorm.DB
}

// NewSubscriptionPlanRepository создает новый репозиторий тарифных планов
func NewSubscriptionPlanRepository(db *DB) *SubscriptionPlanRepository {
	return &SubscriptionPlanRepository{db: db.GetDB()}
}

// GetAll возвращает все активные тарифные планы
func (r *SubscriptionPlanRepository) GetAll() ([]SubscriptionPlan, error) {
	var plans []SubscriptionPlan
	err := r.db.Where("is_active = true").Order("display_order").Find(&plans).Error
	return plans, err
}

// GetByCode находит тариф по коду
func (r *SubscriptionPlanRepository) GetByCode(code string) (*SubscriptionPlan, error) {
	var plan SubscriptionPlan
	err := r.db.Where("code = ? AND is_active = true", code).First(&plan).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &plan, nil
}
