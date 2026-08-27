package app

import (
	"crypto/sha256"
	"encoding/binary"
)

func commandFingerprint(method, customerID string, number uint64, fields ...uint64) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(customerID))
	var raw [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(raw[:], field)
		h.Write(raw[:])
	}
	binary.BigEndian.PutUint64(raw[:], number)
	h.Write(raw[:])
	return h.Sum(nil)
}
