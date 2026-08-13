package metrics

import (
	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// WrapSealer считает отказы расшифрования client_uuid.
//
// Через Open проходят оба пути расшифровки: диспетчер собирает payload перед
// RPC, выдача ссылок строит VLESS URI. Счёт по каждому в отдельности потерял бы
// третий, когда он появится; комментарий в get_customer_access_links.go как раз
// ссылается на эту метрику как на отсутствующую.
//
// Seal не считается: его отказ означает сломанный ключ на записи, а метрика
// собирает именно ошибки расшифрования. Провалившийся Seal валит команду целиком
// и виден как ошибка RPC, тогда как провалившийся Open деградирует молча — гасит
// одну ссылку и ретраит одну операцию.
func (r *Registry) WrapSealer(inner app.CredentialSealer) app.CredentialSealer {
	return &observedSealer{inner: inner, reg: r}
}

type observedSealer struct {
	inner app.CredentialSealer
	reg   *Registry
}

func (s *observedSealer) Seal(uuid crypto.ClientUUID) (crypto.SealedCredential, error) {
	return s.inner.Seal(uuid)
}

func (s *observedSealer) Open(credential crypto.SealedCredential) (crypto.ClientUUID, error) {
	uuid, err := s.inner.Open(credential)
	if err != nil {
		s.reg.credentialOpenErrors.Inc()
	}
	return uuid, err
}

func (s *observedSealer) KeyID() string { return s.inner.KeyID() }
