package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

// goldenDigest — digest примера validSnapshot.
//
// Литерал здесь пиннит канонический payload побайтово: digest — SHA-256 ровно от
// него, и любое изменение раскладки, порядка полей или экранирования сдвинет эту
// строку. Менять её можно только осознанно и понимая, что все уже сохранённые в
// manifest_revisions digest станут несравнимыми с вновь вычисляемыми.
const goldenDigest = "ac2e6ab0d7a84fa83a4f5b681bba638d0b80eeee4537452564e4f5ab0a236300"

func TestCanonicalizeManifestGolden(t *testing.T) {
	payload, digest := CanonicalizeManifest(validSnapshot())

	if digest != goldenDigest {
		t.Fatalf("digest %q, ожидался %q\npayload: %s", digest, goldenDigest, payload)
	}

	// Digest обязан быть SHA-256 именно того payload, который уедет в колонку:
	// иначе сохранённое значение нечем перепроверить.
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatal("digest не является SHA-256 возвращённого payload")
	}
}

// TestCanonicalizeManifestExcludesRevision — rollback выполняется ПРЕЖНИМ
// desired snapshot под новой большей revision. Это работает только если revision
// в digest не входит.
func TestCanonicalizeManifestExcludesRevision(t *testing.T) {
	first := validSnapshot()
	second := validSnapshot()
	second.Revision = first.Revision + 100

	_, digestFirst := CanonicalizeManifest(first)
	_, digestSecond := CanonicalizeManifest(second)

	if digestFirst != digestSecond {
		t.Fatal("digest зависит от revision: rollback прежним снапшотом стал бы невозможен")
	}

	payload, _ := CanonicalizeManifest(first)
	if strings.Contains(string(payload), "revision") {
		t.Fatalf("revision попала в канонический payload: %s", payload)
	}
}

// TestCanonicalizeManifestIgnoresInputOrder — два снапшота, отличающиеся только
// порядком обхода у CI, описывают одну топологию и обязаны дать один digest.
// Иначе повтор того же манифеста отвергался бы как конфликт digest.
func TestCanonicalizeManifestIgnoresInputOrder(t *testing.T) {
	straight := validSnapshot()

	shuffled := validSnapshot()
	slices.Reverse(shuffled.Nodes)
	slices.Reverse(shuffled.Fleets[0].NodeIDs)
	shuffled.Fleets = append(shuffled.Fleets, ManifestFleet{FleetID: 11})
	slices.Reverse(shuffled.Fleets)

	straight.Fleets = append(straight.Fleets, ManifestFleet{FleetID: 11})

	_, digestStraight := CanonicalizeManifest(straight)
	_, digestShuffled := CanonicalizeManifest(shuffled)

	if digestStraight != digestShuffled {
		t.Fatal("digest зависит от порядка элементов во входе")
	}
}

// TestCanonicalizeManifestSeesContentChange — обратная сторона: содержательное
// изменение обязано двигать digest, иначе конфликт не обнаружится.
func TestCanonicalizeManifestSeesContentChange(t *testing.T) {
	base := validSnapshot()
	_, digestBase := CanonicalizeManifest(base)

	changes := map[string]func(*ManifestSnapshot){
		"адрес ноды":        func(s *ManifestSnapshot) { s.Nodes[0].Public.Address = "other.example.com" },
		"endpoint агента":   func(s *ManifestSnapshot) { s.Nodes[0].Agent.Endpoint = "10.0.0.99:9443" },
		"display_name ноды": func(s *ManifestSnapshot) { s.Nodes[0].Public.DisplayName = "Другое имя" },
		"egress_tag связи":  func(s *ManifestSnapshot) { s.Fleets[0].Bridges[0].EgressTag = "other-exit" },
		"состав fleet":      func(s *ManifestSnapshot) { s.Fleets[0].NodeIDs = []NodeID{"NL-1"} },
		"новый fleet":       func(s *ManifestSnapshot) { s.Fleets = append(s.Fleets, ManifestFleet{FleetID: 11}) },
	}

	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			snapshot := validSnapshot()
			mutate(&snapshot)

			if _, digest := CanonicalizeManifest(snapshot); digest == digestBase {
				t.Fatal("изменение не отразилось на digest")
			}
		})
	}
}
