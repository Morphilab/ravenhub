package store

import (
	"testing"

	"ravenhub/internal/crypto"
)

// Dual-read KDF legacy: una cuenta cifrada con la derivación vieja (legacy) sigue
// legible tras subir la KDF a PBKDF2; lo nuevo usa MasterKey.
func TestDecryptCredsLegacyFallback(t *testing.T) {
	newKey := crypto.MasterKey("test-passphrase")
	midKey := crypto.MasterKeyV1("test-passphrase")
	legacyKey := crypto.LegacyMasterKey("test-passphrase")
	s := &Store{MasterKey: newKey, MidMasterKey: midKey, LegacyMasterKey: legacyKey}

	// filas de la era v1 (PBKDF2 210k) siguen legibles tras subir a v2
	v1Blob, err := crypto.EncryptSecret(midKey, []byte(`{"url":"https://v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := s.DecryptCreds(Account{CredsEncrypted: v1Blob}); err != nil || string(pt) != `{"url":"https://v1"}` {
		t.Fatalf("fallback KDF v1: %v %q", err, pt)
	}

	// el formato actual v3 (byte de versión) abre con la clave actual
	v3Blob, err := crypto.EncryptSecretV3(newKey, []byte(`{"url":"https://v3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := s.DecryptCreds(Account{CredsEncrypted: v3Blob}); err != nil || string(pt) != `{"url":"https://v3"}` {
		t.Fatalf("blob v3: %v %q", err, pt)
	}

	oldBlob, err := crypto.EncryptSecret(legacyKey, []byte(`{"url":"https://x"}`))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := s.DecryptCreds(Account{CredsEncrypted: oldBlob})
	if err != nil || string(pt) != `{"url":"https://x"}` {
		t.Fatalf("fallback legacy: %v %q", err, pt)
	}

	newBlob, err := crypto.EncryptSecret(newKey, []byte(`{"url":"https://y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := s.DecryptCreds(Account{CredsEncrypted: newBlob}); err != nil || string(pt) != `{"url":"https://y"}` {
		t.Fatalf("clave nueva: %v %q", err, pt)
	}

	// sin legacy configurado, lo viejo falla (no silencio)
	s2 := &Store{MasterKey: newKey}
	if _, err := s2.DecryptCreds(Account{CredsEncrypted: oldBlob}); err == nil {
		t.Fatal("sin LegacyMasterKey lo viejo debió fallar")
	}
}
