package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// MagicHeader identifies a Permafrost Zero-Knowledge Encrypted payload (version 1).
var MagicHeader = []byte("PFZK01")

const (
	saltSize       = 16
	nonceSize      = 12
	keySize        = 32 // AES-256
	pbkdf2Iter     = 100000
)

// EnvelopeMetadata provides transparent inspection of an encrypted or plain payload.
type EnvelopeMetadata struct {
	Encrypted  bool   `json:"encrypted"`
	Algorithm  string `json:"algorithm"`
	SaltHex    string `json:"salt_hex,omitempty"`
	PayloadLen int    `json:"payload_len"`
}

// GenerateRecoveryKey creates a high-entropy human-friendly Zero-Knowledge key (e.g., pfk_<32-hex>).
func GenerateRecoveryKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "pfk_" + hex.EncodeToString(b[:]), nil
}

// DeriveKey derives a 256-bit key from passphrase and salt using PBKDF2-HMAC-SHA256 (RFC 2898).
func DeriveKey(passphrase string, salt []byte) []byte {
	return pbkdf2SHA256([]byte(passphrase), salt, pbkdf2Iter, keySize)
}

// ComputeKeyVerification returns a deterministic salt and verification hash to store in config.json
// without ever storing the passphrase or raw key on disk.
func ComputeKeyVerification(passphrase string) (saltHex string, checkHex string, err error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	key := DeriveKey(passphrase, salt)
	check := sha256.Sum256(append([]byte("PFROST_KEY_CHECK:"), key...))
	return hex.EncodeToString(salt), hex.EncodeToString(check[:16]), nil
}

// VerifyPassphrase checks if passphrase matches the stored saltHex and checkHex.
func VerifyPassphrase(passphrase, saltHex, checkHex string) bool {
	if saltHex == "" || checkHex == "" {
		return true
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	key := DeriveKey(passphrase, salt)
	check := sha256.Sum256(append([]byte("PFROST_KEY_CHECK:"), key...))
	return hmac.Equal([]byte(hex.EncodeToString(check[:16])), []byte(checkHex))
}

// Encrypt encrypts plaintext using AES-256-GCM with a freshly generated salt and nonce.
// Output format: "PFZK01" (6B) || Salt (16B) || Nonce (12B) || Ciphertext+Tag
func Encrypt(plaintext []byte, passphrase string) ([]byte, error) {
	if strings.TrimSpace(passphrase) == "" {
		return nil, fmt.Errorf("se requiere una clave o passphrase para el cifrado Zero-Knowledge")
	}

	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	key := DeriveKey(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Authenticate the MagicHeader + Salt as Additional Authenticated Data (AAD)
	aad := append(append([]byte{}, MagicHeader...), salt...)
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)

	out := make([]byte, 0, len(MagicHeader)+saltSize+nonceSize+len(ciphertext))
	out = append(out, MagicHeader...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

// Decrypt authenticates and decrypts a "PFZK01" payload using passphrase.
func Decrypt(data []byte, passphrase string) ([]byte, error) {
	if !IsEncrypted(data) {
		return data, nil
	}
	if strings.TrimSpace(passphrase) == "" {
		return nil, fmt.Errorf("el paquete está cifrado con Zero-Knowledge (🔒 PFZK01): proporciona la clave con --key o PFROST_KEY")
	}

	headerEnd := len(MagicHeader)
	saltEnd := headerEnd + saltSize
	nonceEnd := saltEnd + nonceSize
	if len(data) < nonceEnd+16 {
		return nil, fmt.Errorf("paquete cifrado corrupto o truncado")
	}

	salt := data[headerEnd:saltEnd]
	nonce := data[saltEnd:nonceEnd]
	ciphertext := data[nonceEnd:]

	key := DeriveKey(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	aad := data[:saltEnd]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("clave incorrecta o datos manipulados (fallo de autenticación AES-256-GCM)")
	}
	return plaintext, nil
}

// IsEncrypted checks whether data starts with the Permafrost Zero-Knowledge header "PFZK01".
func IsEncrypted(data []byte) bool {
	return len(data) >= len(MagicHeader)+saltSize+nonceSize+16 && bytes.Equal(data[:len(MagicHeader)], MagicHeader)
}

// Inspect returns transparent metadata about a byte payload or file without needing the secret key.
func Inspect(data []byte) EnvelopeMetadata {
	if IsEncrypted(data) {
		salt := data[len(MagicHeader) : len(MagicHeader)+saltSize]
		return EnvelopeMetadata{
			Encrypted:  true,
			Algorithm:  "AES-256-GCM + PBKDF2-HMAC-SHA256 (Zero-Knowledge PFZK01)",
			SaltHex:    hex.EncodeToString(salt),
			PayloadLen: len(data),
		}
	}
	return EnvelopeMetadata{
		Encrypted:  false,
		Algorithm:  "Plaintext (Sin cifrar)",
		PayloadLen: len(data),
	}
}

// ResolveKey resolves a passphrase from explicit flag, PFROST_KEY environment variable, or default vault key file.
func ResolveKey(explicitKey string) string {
	if strings.TrimSpace(explicitKey) != "" {
		return strings.TrimSpace(explicitKey)
	}
	if envKey := strings.TrimSpace(os.Getenv("PFROST_KEY")); envKey != "" {
		return envKey
	}
	return ""
}

// Standard RFC 2898 PBKDF2-HMAC-SHA256 implementation using only Go standard library crypto/hmac & crypto/sha256.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)

	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		prf.Write(buf[:])
		dk = prf.Sum(dk)
		T := dk[len(dk)-hashLen:]
		copy(U, T)

		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = U[:0]
			U = prf.Sum(U)
			for x := range U {
				T[x] ^= U[x]
			}
		}
	}
	return dk[:keyLen]
}
