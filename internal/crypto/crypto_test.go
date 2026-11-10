package crypto

import (
	"bytes"
	"testing"
)

func TestZeroKnowledgeEncryptDecryptAndTamperRejection(t *testing.T) {
	secret := []byte("terraform.tfstate secret credentials and database dump")
	key, err := GenerateRecoveryKey()
	if err != nil {
		t.Fatalf("GenerateRecoveryKey failed: %v", err)
	}

	// 1. Encrypt
	enc, err := Encrypt(secret, key)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if !IsEncrypted(enc) {
		t.Fatalf("expected IsEncrypted to be true")
	}

	meta := Inspect(enc)
	if !meta.Encrypted || meta.SaltHex == "" {
		t.Fatalf("unexpected envelope metadata: %+v", meta)
	}

	// 2. Decrypt with correct key
	dec, err := Decrypt(enc, key)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if !bytes.Equal(dec, secret) {
		t.Fatalf("decrypted payload does not match original secret")
	}

	// 3. Reject wrong key
	if _, err := Decrypt(enc, "wrong-passphrase"); err == nil {
		t.Fatalf("expected Decrypt to fail with wrong key")
	}

	// 4. Reject 1-bit tampering in ciphertext
	tampered := make([]byte, len(enc))
	copy(tampered, enc)
	tampered[len(tampered)-5] ^= 0x01
	if _, err := Decrypt(tampered, key); err == nil {
		t.Fatalf("expected Decrypt to detect and reject 1-bit tampering")
	}

	// 5. Key verification check
	saltHex, checkHex, err := ComputeKeyVerification(key)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassphrase(key, saltHex, checkHex) {
		t.Fatalf("expected VerifyPassphrase to succeed with matching key")
	}
	if VerifyPassphrase("bad-key", saltHex, checkHex) {
		t.Fatalf("expected VerifyPassphrase to fail with wrong key")
	}
}
