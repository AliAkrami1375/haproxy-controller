package ai

import "testing"

func TestEncryptRoundTrip(t *testing.T) {
	secret := "session-secret-value"
	plain := "sk-or-v1-abcdef0123456789"

	enc, err := Encrypt(plain, secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	if !IsEncrypted(enc) {
		t.Fatal("IsEncrypted did not recognise the ciphertext")
	}

	got, err := Decrypt(enc, secret)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plain {
		t.Errorf("round trip = %q, want %q", got, plain)
	}
}

func TestDecryptWithWrongSecretFails(t *testing.T) {
	enc, _ := Encrypt("top-secret", "correct-secret")
	if _, err := Decrypt(enc, "wrong-secret"); err == nil {
		t.Error("Decrypt succeeded with the wrong secret")
	}
}

func TestDecryptPassesThroughPlaintext(t *testing.T) {
	// A value stored before encryption (or set by hand) must keep working.
	got, err := Decrypt("sk-plain-key", "any-secret")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "sk-plain-key" {
		t.Errorf("got %q, want the value unchanged", got)
	}
}

func TestEmptyValues(t *testing.T) {
	if enc, err := Encrypt("", "s"); err != nil || enc != "" {
		t.Errorf("Encrypt(\"\") = %q, %v", enc, err)
	}
	if dec, err := Decrypt("", "s"); err != nil || dec != "" {
		t.Errorf("Decrypt(\"\") = %q, %v", dec, err)
	}
}

func TestMaskKey(t *testing.T) {
	if got := MaskKey("sk-or-v1-abcd1234"); got != "••••••••1234" {
		t.Errorf("MaskKey = %q", got)
	}
	if got := MaskKey(""); got != "" {
		t.Errorf("MaskKey(\"\") = %q", got)
	}
}
