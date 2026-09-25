package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	Mask       = "••••"
	KeySize    = 32
	version    = 1
	keyIDSize  = 8
	nonceSize  = 12
	headerSize = 1 + keyIDSize + nonceSize
	EnvKey     = "DOLMEN_SECRET_KEY"
	EnvKeyFile = "DOLMEN_SECRET_KEY_FILE"
)

var (
	ErrNoKey    = errors.New("no secret key is configured; set DOLMEN_SECRET_KEY to a base64-encoded 32-byte key (or DOLMEN_SECRET_KEY_FILE to a file holding one) and restart, or pass WithSecretKey to the Go library")
	ErrWrongKey = errors.New("this secret value was encrypted under a different key than the one configured; start the server with the key the value was written under")
	ErrCorrupt  = errors.New("this secret value is not a well-formed dolmen ciphertext; it was changed outside dolmen and cannot be decrypted")
	ErrTampered = errors.New("this secret value failed authentication under the configured key; the stored ciphertext was altered and cannot be decrypted")
)

type Keyring struct {
	aead cipher.AEAD
	id   [keyIDSize]byte
}

func New(key []byte) (*Keyring, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("a secret key must be exactly %d bytes, got %d; generate one with: openssl rand -base64 32", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	k := &Keyring{aead: aead}
	sum := sha256.Sum256(key)
	copy(k.id[:], sum[:keyIDSize])
	return k, nil
}

func ParseKey(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("the secret key is not valid base64; generate one with: openssl rand -base64 32")
	}
	if len(raw) != KeySize {
		return nil, fmt.Errorf("the secret key must decode to exactly %d bytes, got %d; generate one with: openssl rand -base64 32", KeySize, len(raw))
	}
	return raw, nil
}

func LoadKey(getenv func(string) string) ([]byte, error) {
	inline, file := getenv(EnvKey), getenv(EnvKeyFile)
	switch {
	case inline != "" && file != "":
		return nil, fmt.Errorf("set only one of %s and %s", EnvKey, EnvKeyFile)
	case inline != "":
		k, err := ParseKey(inline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvKey, err)
		}
		return k, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read the key file: %w", EnvKeyFile, err)
		}
		k, err := ParseKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvKeyFile, err)
		}
		return k, nil
	}
	return nil, nil
}

func (k *Keyring) Seal(plaintext string) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	out := make([]byte, headerSize, headerSize+len(plaintext)+k.aead.Overhead())
	out[0] = version
	copy(out[1:], k.id[:])
	nonce := out[1+keyIDSize : headerSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(out, nonce, []byte(plaintext), out[:1+keyIDSize]), nil
}

func (k *Keyring) Open(blob []byte) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	if len(blob) < headerSize+k.aead.Overhead() || blob[0] != version {
		return "", ErrCorrupt
	}
	if !bytes.Equal(blob[1:1+keyIDSize], k.id[:]) {
		return "", ErrWrongKey
	}
	plain, err := k.aead.Open(nil, blob[1+keyIDSize:headerSize], blob[headerSize:], blob[:1+keyIDSize])
	if err != nil {
		return "", ErrTampered
	}
	return string(plain), nil
}
