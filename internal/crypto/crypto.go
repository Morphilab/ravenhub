// Package crypto implements RavenHub's secret handling:
// API tokens stored as SHA-256 hashes, and account credentials
// encrypted at rest with AES-256-GCM.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// NewToken generates an API token `rvn_<project>_<32 hex>`.
// The full value is shown exactly once; only its SHA-256 hash is stored.
func NewToken(project string) (full, prefix string, err error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", "", fmt.Errorf("generando aleatorio: %w", err)
	}
	secret := hex.EncodeToString(raw)
	slug := sanitizeSlug(project)
	full = fmt.Sprintf("rvn_%s_%s", slug, secret)
	// el prefijo es solo identificación estética (listados, auditoría): exponer
	// un tramo largo del secreto recortaba el espacio de búsqueda del token —
	// 4 chars (16 bits) bastan para reconocerlo a simple vista
	prefix = fmt.Sprintf("rvn_%s_%s", slug, secret[:4])
	return full, prefix, nil
}

// NewAdminToken generates an admin token `rva_admin_<32 hex>`.
func NewAdminToken() (full, prefix string, err error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", "", fmt.Errorf("generando aleatorio: %w", err)
	}
	secret := hex.EncodeToString(raw)
	full = fmt.Sprintf("rva_admin_%s", secret)
	prefix = "rva_admin_" + secret[:4]
	return full, prefix, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func sanitizeSlug(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "")
	if s == "" {
		s = "proj"
	}
	return s
}

// HashToken returns the hex SHA-256 of a token — the only stored form.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MasterKey derives the 32-byte AES key from the configured passphrase with
// PBKDF2-HMAC-SHA256 (600k iteraciones — recomendación OWASP 2023 —, sal fija
// de aplicación v2). La sal es fija (documentada): el secreto real es la
// passphrase por despliegue (openssl rand -hex 32); el coste de 600k encarece
// un ataque de diccionario.
func MasterKey(passphrase string) []byte {
	return pbkdf2SHA256([]byte(passphrase), []byte("ravenhub-kdf-v2"), 600000, 32)
}

// MasterKeyV1 is the legacy PBKDF2 derivation (210k, sal v1). Solo para el
// dual-read de filas cifradas con versiones anteriores; las filas nuevas se
// cifran con MasterKey y un re-import re-cifra las viejas.
// No usar para cifrar nada nuevo.
func MasterKeyV1(passphrase string) []byte {
	return pbkdf2SHA256([]byte(passphrase), []byte("ravenhub-kdf-v1"), 210000, 32)
}

// LegacyMasterKey is the legacy derivation (SHA256 directo). Solo para
// dual-read de filas cifradas antes de la migración: las nuevas filas usan
// MasterKey. No usar para cifrar nada nuevo.
func LegacyMasterKey(passphrase string) []byte {
	sum := sha256.Sum256([]byte(passphrase))
	return sum[:]
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var buf [4]byte
	for b := 1; b <= blocks; b++ {
		buf[0], buf[1], buf[2], buf[3] = byte(b>>24), byte(b>>16), byte(b>>8), byte(b)
		prf.Reset()
		prf.Write(salt)
		prf.Write(buf[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			subtle.XORBytes(t, t, u)
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// EncryptSecret seals plaintext with AES-256-GCM: nonce(12) || ciphertext.
func EncryptSecret(master []byte, plaintext []byte) ([]byte, error) {
	gcm, err := gcm(master)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generando nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// BlobVersionV3 es el byte de versión del formato actual de blobs cifrados:
// [0x03] || nonce(12) || ciphertext. Permite detectar sin ambigüedad un
// cambio de formato/KDF y distinguirlo de un blob corrupto.
const BlobVersionV3 = 0x03

// EncryptSecretV3 cifra con el formato con versión (el que escribe store).
func EncryptSecretV3(master []byte, plaintext []byte) ([]byte, error) {
	sealed, err := EncryptSecret(master, plaintext)
	if err != nil {
		return nil, err
	}
	return append([]byte{BlobVersionV3}, sealed...), nil
}

// DecryptSecretV3 abre un blob del formato v3. Un blob sin el byte de versión
// (formatos previos) se rechaza aquí — el fallback de store prueba los
// formatos antiguos explícitamente, así que un nonce viejo que empiece por
// 0x03 no se malinterpreta: el intento v3 falla en la autenticación GCM y la
// cadena sigue con el layout antiguo.
func DecryptSecretV3(master, sealed []byte) ([]byte, error) {
	if len(sealed) == 0 || sealed[0] != BlobVersionV3 {
		return nil, errors.New("blob sin versión v3")
	}
	return DecryptSecret(master, sealed[1:])
}

// DecryptSecret opens a value produced by EncryptSecret.
func DecryptSecret(master, sealed []byte) ([]byte, error) {
	gcm, err := gcm(master)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("blob cifrado demasiado corto")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func gcm(master []byte) (cipher.AEAD, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("master key debe tener 32 bytes, tiene %d", len(master))
	}
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
