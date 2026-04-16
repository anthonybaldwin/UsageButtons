//go:build ignore

// Generate an RSA keypair for the Usage Buttons Chrome extension and
// print:
//   - the base64-encoded SubjectPublicKeyInfo (the `key` field for
//     manifest.json — this pins the extension ID)
//   - the derived deterministic extension ID
//   - a PEM-encoded private key written to
//     ./chrome-extension-private.pem (gitignored)
//
// Run once when bootstrapping the extension — the generated ID should
// be committed to cookies.DefaultExtensionID and manifest.json. The
// private key stays local; it's only needed for Chrome Web Store
// uploads down the road.
//
// Usage:
//   go run scripts/gen-extension-key.go
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log"
	"os"
)

const privateKeyPath = "chrome-extension-private.pem"

func main() {
	if _, err := os.Stat(privateKeyPath); err == nil {
		log.Fatalf("%s already exists — refusing to overwrite. Delete it to regenerate.", privateKeyPath)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		log.Fatalf("marshal public key: %v", err)
	}

	// Chrome extension ID: SHA-256 of DER-encoded SubjectPublicKeyInfo,
	// take the first 16 bytes (32 hex chars), map each hex nibble
	// 0x0-0xF to 'a'-'p'.
	sum := sha256.Sum256(pubDER)
	const alphabet = "abcdefghijklmnop"
	id := make([]byte, 32)
	for i := 0; i < 16; i++ {
		id[i*2] = alphabet[sum[i]>>4]
		id[i*2+1] = alphabet[sum[i]&0xf]
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(privateKeyPath, privPEM, 0o600); err != nil {
		log.Fatalf("write private key: %v", err)
	}

	pubB64 := base64.StdEncoding.EncodeToString(pubDER)

	fmt.Println()
	fmt.Println("Extension ID (deterministic):")
	fmt.Println("  " + string(id))
	fmt.Println()
	fmt.Println("Public key for manifest.json (field \"key\"):")
	fmt.Println("  " + pubB64)
	fmt.Println()
	fmt.Println("Private key written to: " + privateKeyPath + " (gitignored).")
	fmt.Println("Keep this file — Chrome Web Store upload needs it later.")
}
