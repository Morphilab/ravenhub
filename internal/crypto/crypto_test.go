package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewTokenFormat(t *testing.T) {
	full, prefix, err := NewToken("proyecto1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "rvn_proyecto1_") {
		t.Fatalf("formato inesperado: %s", full)
	}
	if len(full) != len("rvn_proyecto1_")+32 {
		t.Fatalf("longitud inesperada: %d", len(full))
	}
	if !strings.HasPrefix(full, prefix) {
		t.Fatalf("prefix %q no es prefijo de %q", prefix, full)
	}
	// el prefijo no debe exponer un tramo largo del secreto (solo 4 hex)
	if len(prefix) > len("rvn_")+len("proyecto1")+1+4 {
		t.Fatalf("prefix expone demasiado secreto: %q", prefix)
	}
	// slug sanitizado
	full2, _, _ := NewToken("Proyecto ¡Raro! 2")
	if !strings.HasPrefix(full2, "rvn_proyectoraro2_") {
		t.Fatalf("slug no sanitizado: %s", full2)
	}
}

func TestHashTokenVerify(t *testing.T) {
	full, _, _ := NewToken("p1")
	h := HashToken(full)
	if h == full || len(h) != 64 {
		t.Fatalf("hash inválido: %s", h)
	}
	if h != HashToken(full) {
		t.Fatal("hash del mismo token no coincide")
	}
	if h == HashToken(full+"x") {
		t.Fatal("hash de token distinto coincide")
	}
}

func TestEncryptSecretRoundTrip(t *testing.T) {
	master := MasterKey("clave-maestra-de-prueba")
	plain := []byte(`{"bot_token":"123:ABC"}`)
	sealed, err := EncryptSecret(master, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed, plain) {
		t.Fatal("el blob cifrado contiene el texto en claro")
	}
	got, err := DecryptSecret(master, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip roto: %s", got)
	}
	// otra clave no abre
	if _, err := DecryptSecret(MasterKey("otra-clave"), sealed); err == nil {
		t.Fatal("abrió con clave incorrecta")
	}
	// bit flip → GCM rechaza
	sealed[5] ^= 0xFF
	if _, err := DecryptSecret(master, sealed); err == nil {
		t.Fatal("blob manipulado aceptado")
	}
}

func TestMasterKeyDerivation(t *testing.T) {
	k1 := MasterKey("dev-master-key-cambiar-en-produccion")
	k2 := MasterKey("dev-master-key-cambiar-en-produccion")
	if len(k1) != 32 || !bytes.Equal(k1, k2) {
		t.Fatal("KDF no determinista o longitud incorrecta")
	}
	legacy := LegacyMasterKey("dev-master-key-cambiar-en-produccion")
	if bytes.Equal(k1, legacy) {
		t.Fatal("la KDF nueva coincide con la derivación vieja (sin mejora)")
	}
	// roundtrip con la clave nueva
	ct, err := EncryptSecret(k1, []byte(`{"bot_token":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := DecryptSecret(k1, ct)
	if err != nil || string(pt) != `{"bot_token":"x"}` {
		t.Fatalf("roundtrip KDF: %v %q", err, pt)
	}
	// la clave vieja NO abre lo cifrado con la nueva
	if _, err := DecryptSecret(legacy, ct); err == nil {
		t.Fatal("la clave legacy abre material nuevo (claves no separadas)")
	}
}

func TestMasterKeyV1DiffersAndRoundTrips(t *testing.T) {
	// v2 (600k, sal v2) y v1 (210k, sal v1) deben ser claves distintas y
	// ambas deterministas de 32 bytes — el dual-read de store depende de ello.
	v2 := MasterKey("passphrase-de-prueba")
	v1 := MasterKeyV1("passphrase-de-prueba")
	if len(v2) != 32 || len(v1) != 32 {
		t.Fatalf("longitudes incorrectas: v2=%d v1=%d", len(v2), len(v1))
	}
	if bytes.Equal(v1, v2) {
		t.Fatal("las KDF v1 y v2 derivan la misma clave (la sal de versión no separa)")
	}
	if !bytes.Equal(v1, MasterKeyV1("passphrase-de-prueba")) {
		t.Fatal("KDF v1 no determinista")
	}
	ct, err := EncryptSecret(v1, []byte("material-viejo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptSecret(v2, ct); err == nil {
		t.Fatal("la KDF v2 abre material de v1 (las filas viejas no quedarían protegidas por el dual-read)")
	}
}

func TestEncryptSecretV3Format(t *testing.T) {
	key := MasterKey("pass-v3")
	blob, err := EncryptSecretV3(key, []byte("secreto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) == 0 || blob[0] != BlobVersionV3 {
		t.Fatalf("sin byte de versión v3: % x", blob[:min(len(blob), 4)])
	}
	if pt, err := DecryptSecretV3(key, blob); err != nil || string(pt) != "secreto" {
		t.Fatalf("roundtrip v3: %v %q", err, pt)
	}
	// un blob SIN versión (layout antiguo) no se acepta como v3: la cadena de
	// fallback de store prueba los layouts viejos explícitamente
	old, err := EncryptSecret(key, []byte("viejo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptSecretV3(key, old); err == nil {
		t.Fatal("blob sin byte de versión aceptado por DecryptSecretV3")
	}
	if pt, err := DecryptSecret(key, old); err != nil || string(pt) != "viejo" {
		t.Fatalf("layout antiguo roto: %v %q", err, pt)
	}
}
