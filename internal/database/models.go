package database

import (
	"time"
)

// User представляет пользователя системы.
// Основная модель для хранения информации о клиентах VPN сервиса.
//
// Поля:
//   - ID: уникальный идентификатор пользователя в системе
//   - TelegramID: уникальный идентификатор пользователя в Telegram (обязательное поле, используется для аутентификации)
//   - Username: имя пользователя в Telegram
//   - Email: адрес электронной почты для уведомлений и восстановления доступа
//   - CreatedAt: дата и время регистрации пользователя
//   - UpdatedAt: дата и время последнего обновления данных
//
// Связи:
//   - Subscriptions: все подписки пользователя (может иметь несколько: активные, истекшие, отмененные)
//   - VPNConfigs: все VPN конфигурации пользователя для разных серверов и устройств
//   - Payments: история всех платежей пользователя
//   - TrafficStats: статистика использования трафика по дням
type User struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	TelegramID int64     `gorm:"uniqueIndex;not null" json:"telegram_id"`
	Username   string    `gorm:"size:255" json:"username"`
	Email      string    `gorm:"size:255" json:"email"`
	CreatedAt  time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt  time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Связи
	Subscriptions []Subscription `gorm:"foreignKey:UserID" json:"subscriptions,omitempty"`
	VPNConfigs    []VPNConfig    `gorm:"foreignKey:UserID" json:"vpn_configs,omitempty"`
	Payments      []Payment      `gorm:"foreignKey:UserID" json:"payments,omitempty"`
	TrafficStats  []TrafficStat  `gorm:"foreignKey:UserID" json:"traffic_stats,omitempty"`
}

// TableName задает имя таблицы для User
func (User) TableName() string {
	return "users"
}

// Subscription представляет подписку пользователя на VPN сервис.
// Содержит информацию о тарифном плане, датах действия и автопродлении.
//
// Поля:
//   - ID: уникальный идентификатор подписки
//   - UserID: идентификатор пользователя-владельца подписки
//   - PlanType: тип тарифного плана (basic, premium, premium_year)
//   - StartDate: дата начала действия подписки
//   - EndDate: дата окончания действия подписки
//   - IsActive: флаг активности подписки (автоматически обновляется при истечении срока)
//   - AutoRenew: флаг автоматического продления подписки при окончании срока
//   - CreatedAt: дата и время создания подписки
//
// Связи:
//   - User: пользователь-владелец подписки
//   - VPNConfigs: VPN конфигурации, привязанные к данной подписке
//   - Payments: платежи, связанные с этой подпиской (начальная оплата и продления)
//
// Методы:
//   - IsExpired(): проверяет истекла ли подписка на текущий момент
//   - DaysLeft(): возвращает количество дней до истечения подписки
type Subscription struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"not null;index" json:"user_id"`
	PlanType  string    `gorm:"size:50;not null" json:"plan_type"` // basic, premium, premium_year
	StartDate time.Time `gorm:"not null" json:"start_date"`
	EndDate   time.Time `gorm:"not null" json:"end_date"`
	IsActive  bool      `gorm:"default:true;index" json:"is_active"`
	AutoRenew bool      `gorm:"default:false" json:"auto_renew"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`

	// Связи
	User       User        `gorm:"foreignKey:UserID" json:"user,omitempty"`
	VPNConfigs []VPNConfig `gorm:"foreignKey:SubscriptionID" json:"vpn_configs,omitempty"`
	Payments   []Payment   `gorm:"foreignKey:SubscriptionID" json:"payments,omitempty"`
}

// TableName задает имя таблицы для Subscription
func (Subscription) TableName() string {
	return "subscriptions"
}

// IsExpired проверяет, истекла ли подписка
func (s *Subscription) IsExpired() bool {
	return time.Now().After(s.EndDate)
}

// DaysLeft возвращает количество оставшихся дней подписки
func (s *Subscription) DaysLeft() int {
	if s.IsExpired() {
		return 0
	}
	duration := time.Until(s.EndDate)
	return int(duration.Hours() / 24)
}

// VPNConfig представляет конфигурацию VPN для пользователя.
// Содержит VLESS UUID и настройки для подключения к VPN серверу.
//
// Поля:
//   - ID: уникальный идентификатор конфигурации
//   - UserID: идентификатор пользователя-владельца конфигурации
//   - SubscriptionID: идентификатор подписки, к которой привязана конфигурация
//   - ServerID: идентификатор VPN сервера, для которого создана конфигурация
//   - UUID: уникальный идентификатор пользователя VLESS (используется для аутентификации)
//   - Flow: настройки потока XTLS (например, "xtls-rprx-vision")
//   - CreatedAt: дата и время создания конфигурации
//   - UpdatedAt: дата и время последнего обновления
//
// Связи:
//   - User: пользователь-владелец конфигурации
//   - Subscription: подписка, к которой привязана конфигурация
//   - Server: VPN сервер, к которому относится конфигурация
//   - TrafficStats: статистика использования трафика для данной конфигурации
//
// Безопасность:
//   - UUID имеет уникальный индекс для предотвращения дублирования
type VPNConfig struct {
	ID             uint `gorm:"primaryKey" json:"id"`
	UserID         uint `gorm:"not null;index" json:"user_id"`
	SubscriptionID uint `gorm:"not null;index" json:"subscription_id"`
	ServerID       uint `gorm:"not null;index" json:"server_id"`

	// VLESS использует UUID для идентификации пользователя
	UUID string `gorm:"type:uuid;not null;uniqueIndex" json:"uuid"`

	// Flow специфичен для VLESS с XTLS (например, "xtls-rprx-vision")
	Flow string `gorm:"default:'xtls-rprx-vision'" json:"flow"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Связи
	User         User          `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Subscription Subscription  `gorm:"foreignKey:SubscriptionID" json:"subscription,omitempty"`
	Server       VPNServer     `gorm:"foreignKey:ServerID" json:"server,omitempty"`
	TrafficStats []TrafficStat `gorm:"foreignKey:ConfigID" json:"traffic_stats,omitempty"`
}

// TableName задает имя таблицы для VPNConfig
func (VPNConfig) TableName() string {
	return "vpn_configs"
}

// VPNServer представляет VPN сервер в определенной географической локации.
// Используется для балансировки нагрузки и выбора оптимального сервера для клиентов.
//
// Поля:
//   - ID: уникальный идентификатор сервера
//   - Name: уникальное имя сервера (например: "Germany-Frankfurt-1")
//   - Host: IP адрес или доменное имя сервера
//   - Port: порт для WireGuard подключений (по умолчанию 51820)
//   - PublicKey: публичный WireGuard ключ сервера
//   - Location: название локации (например: "Frankfurt", "New York")
//   - CountryCode: двухбуквенный код страны ISO 3166-1 (например: "DE", "US")
//   - IsActive: флаг активности сервера (неактивные серверы не используются для новых подключений)
//   - MaxUsers: максимальное количество одновременных пользователей
//   - CurrentUsers: текущее количество подключенных пользователей
//   - LoadPercent: процент загрузки сервера (рассчитывается как CurrentUsers/MaxUsers * 100)
//   - CreatedAt: дата и время добавления сервера в систему
//   - UpdatedAt: дата и время последнего обновления данных сервера
//
// Связи:
//   - VPNConfigs: все конфигурации, привязанные к данному серверу
//
// Методы:
//   - HasCapacity(): проверяет есть ли свободные места на сервере
//   - UpdateLoad(): пересчитывает процент загрузки сервера
type VPNServer struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Name         string    `gorm:"size:255;not null;uniqueIndex" json:"name"`
	Host         string    `gorm:"size:255;not null" json:"host"`
	Port         int       `gorm:"not null" json:"port"`
	PublicKey    string    `gorm:"type:text;not null" json:"public_key"`
	Location     string    `gorm:"size:100" json:"location"`   // Frankfurt, New York, etc.
	CountryCode  string    `gorm:"size:2" json:"country_code"` // DE, US, etc.
	IsActive     bool      `gorm:"default:true;index" json:"is_active"`
	MaxUsers     int       `gorm:"default:1000" json:"max_users"`
	CurrentUsers int       `gorm:"default:0" json:"current_users"`
	LoadPercent  float64   `gorm:"default:0" json:"load_percent"` // Процент загрузки сервера
	CreatedAt    time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Связи
	VPNConfigs []VPNConfig `gorm:"foreignKey:ServerID" json:"vpn_configs,omitempty"`
}

// TableName задает имя таблицы для VPNServer
func (VPNServer) TableName() string {
	return "vpn_servers"
}

// HasCapacity проверяет, есть ли свободные места на сервере
func (s *VPNServer) HasCapacity() bool {
	return s.CurrentUsers < s.MaxUsers
}

// UpdateLoad обновляет процент загрузки сервера
func (s *VPNServer) UpdateLoad() {
	if s.MaxUsers > 0 {
		s.LoadPercent = float64(s.CurrentUsers) / float64(s.MaxUsers) * 100
	}
}

// Payment представляет платеж пользователя.
// Хранит информацию о транзакциях, их статусах и связанных подписках.
//
// Поля:
//   - ID: уникальный идентификатор платежа
//   - UserID: идентификатор пользователя, совершившего платеж
//   - SubscriptionID: идентификатор подписки (может быть nil для разовых платежей)
//   - Amount: сумма платежа в валюте Currency
//   - Currency: код валюты (RUB, USD, EUR)
//   - Status: статус платежа (pending, processing, succeeded, failed, cancelled)
//   - PaymentMethod: способ оплаты (yookassa, stripe, crypto)
//   - TransactionID: уникальный идентификатор транзакции в платежной системе
//   - CreatedAt: дата и время создания платежа
//
// Связи:
//   - User: пользователь, совершивший платеж
//   - Subscription: подписка, для которой совершен платеж (опционально)
//
// Методы:
//   - IsSuccessful(): проверяет успешно ли завершен платеж (status == "succeeded")
//   - IsPending(): проверяет находится ли платеж в ожидании (status == "pending")
//
// Статусы платежей:
//   - pending: платеж создан, ожидает обработки
//   - processing: платеж обрабатывается платежной системой
//   - succeeded: платеж успешно завершен
//   - failed: платеж отклонен или произошла ошибка
//   - cancelled: платеж отменен пользователем
type Payment struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	UserID         uint      `gorm:"not null;index" json:"user_id"`
	SubscriptionID *uint     `gorm:"index" json:"subscription_id"` // Может быть null для первого платежа
	Amount         float64   `gorm:"type:decimal(10,2);not null" json:"amount"`
	Currency       string    `gorm:"size:3;default:'RUB'" json:"currency"`
	Status         string    `gorm:"size:50;not null;index" json:"status"` // pending, processing, succeeded, failed, cancelled
	PaymentMethod  string    `gorm:"size:50" json:"payment_method"`        // yookassa, stripe, crypto
	TransactionID  string    `gorm:"size:255;uniqueIndex" json:"transaction_id"`
	Metadata       string    `gorm:"type:text" json:"metadata"` // JSON для дополнительных данных
	CreatedAt      time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Связи
	User         User          `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Subscription *Subscription `gorm:"foreignKey:SubscriptionID" json:"subscription,omitempty"`
}

// TableName задает имя таблицы для Payment
func (Payment) TableName() string {
	return "payments"
}

// IsSuccessful проверяет, успешен ли платеж
func (p *Payment) IsSuccessful() bool {
	return p.Status == "succeeded"
}

// IsPending проверяет, ожидает ли платеж обработки
func (p *Payment) IsPending() bool {
	return p.Status == "pending" || p.Status == "processing"
}

// TrafficStat представляет статистику использования трафика.
// Собирается ежедневно для биллинга, мониторинга и аналитики использования сервиса.
//
// Поля:
//   - ID: уникальный идентификатор записи статистики
//   - UserID: идентификатор пользователя
//   - ConfigID: идентификатор VPN конфигурации, для которой собрана статистика
//   - Date: дата, за которую собрана статистика (используется для группировки)
//   - BytesIn: количество байт входящего трафика (загрузка)
//   - BytesOut: количество байт исходящего трафика (отдача)
//   - CreatedAt: дата и время создания записи
//
// Связи:
//   - User: пользователь, к которому относится статистика
//   - Config: VPN конфигурация, для которой собрана статистика
//
// Методы:
//   - TotalBytes(): возвращает общий объем трафика в байтах (BytesIn + BytesOut)
//   - TotalGB(): возвращает общий объем трафика в гигабайтах с округлением до 2 знаков
//
// Индексы:
//   - Композитный индекс по (user_id, date) для быстрого получения статистики пользователя за период
type TrafficStat struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	UserID        uint      `gorm:"not null;index:idx_user_date" json:"user_id"`
	ConfigID      uint      `gorm:"not null;index" json:"config_id"`
	BytesSent     int64     `gorm:"default:0" json:"bytes_sent"`
	BytesReceived int64     `gorm:"default:0" json:"bytes_received"`
	Date          time.Time `gorm:"type:date;not null;index:idx_user_date" json:"date"`
	CreatedAt     time.Time `gorm:"autoCreateTime" json:"created_at"`

	// Связи
	User   User      `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Config VPNConfig `gorm:"foreignKey:ConfigID" json:"config,omitempty"`
}

// TableName задает имя таблицы для TrafficStat
func (TrafficStat) TableName() string {
	return "traffic_stats"
}

// TotalBytes возвращает общий объем трафика
func (t *TrafficStat) TotalBytes() int64 {
	return t.BytesSent + t.BytesReceived
}

// TotalGB возвращает общий трафик в гигабайтах
func (t *TrafficStat) TotalGB() float64 {
	return float64(t.TotalBytes()) / (1024 * 1024 * 1024)
}

// SubscriptionPlan представляет тарифный план VPN сервиса.
// Используется для отображения доступных планов в Telegram боте и на сайте.
// Данные тарифных планов загружаются при инициализации базы данных через seedDefaultData().
//
// Поля:
//   - ID: уникальный идентификатор тарифного плана
//   - Code: уникальный код плана (basic, premium, premium_year)
//   - Name: отображаемое название плана (например: "Премиум на год")
//   - Description: подробное описание возможностей плана
//   - Price: стоимость плана в рублях
//   - Currency: валюта цены (по умолчанию RUB)
//   - DurationDays: длительность подписки в днях (30 для месяца, 365 для года)
//   - MaxDevices: максимальное количество устройств
//   - SpeedLimit: ограничение скорости в Мбит/с (0 = без ограничений)
//   - IsActive: флаг активности плана (неактивные планы не отображаются пользователям)
//   - CreatedAt: дата и время создания плана
//   - UpdatedAt: дата и время последнего обновления
//
// Стандартные планы:
//   - Basic: 299₽/мес, 1 устройство, 50 Мбит/с
//   - Premium: 599₽/мес, 5 устройств, безлимит
//   - Premium Year: 5990₽/год, 5 устройств, безлимит (экономия 16%)
type SubscriptionPlan struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Name         string    `gorm:"size:100;not null;uniqueIndex" json:"name"` // Basic, Premium, Premium Year
	Code         string    `gorm:"size:50;not null;uniqueIndex" json:"code"`  // basic, premium, premium_year
	DurationDays int       `gorm:"not null" json:"duration_days"`
	Price        float64   `gorm:"type:decimal(10,2);not null" json:"price"`
	Currency     string    `gorm:"size:3;default:'RUB'" json:"currency"`
	MaxDevices   int       `gorm:"default:1" json:"max_devices"`
	MaxSpeed     int       `gorm:"default:0" json:"max_speed"` // Мбит/с, 0 = безлимит
	Description  string    `gorm:"type:text" json:"description"`
	Features     string    `gorm:"type:text" json:"features"` // JSON массив фич
	IsActive     bool      `gorm:"default:true" json:"is_active"`
	DisplayOrder int       `gorm:"default:0" json:"display_order"`
	CreatedAt    time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName задает имя таблицы для SubscriptionPlan
func (SubscriptionPlan) TableName() string {
	return "subscription_plans"
}
