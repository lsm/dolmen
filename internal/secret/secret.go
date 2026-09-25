package secret

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	Mask           = "••••"
	KeySize        = 32
	version        = 1
	keyIDSize      = 8
	nonceSize      = 12
	headerSize     = 1 + keyIDSize + nonceSize
	EnvKey         = "DOLMEN_SECRET_KEY"
	EnvKeyFile     = "DOLMEN_SECRET_KEY_FILE"
	EnvOldKeys     = "DOLMEN_SECRET_KEYS_OLD"
	EnvOldKeysFile = "DOLMEN_SECRET_KEYS_OLD_FILE"
)

var (
	ErrNoKey    = errors.New("no secret key is configured; set DOLMEN_SECRET_KEY to a base64-encoded 32-byte key (or DOLMEN_SECRET_KEY_FILE to a file holding one) and restart, or pass WithSecretKey to the Go library")
	ErrWrongKey = errors.New("this secret value was encrypted under a different key than the one configured")
	ErrCorrupt  = errors.New("this secret value is not a well-formed dolmen ciphertext; it was changed outside dolmen and cannot be decrypted")
	ErrTampered = errors.New("this secret value failed authentication under the configured key; the stored ciphertext was altered outside dolmen (possible tampering) and cannot be decrypted")
)

type WrongKeyError struct {
	Stored     string
	Configured string
}

func (e *WrongKeyError) Error() string {
	return fmt.Sprintf("%s: the value was written under key id %s, and the configured keys have ids %s; add the key whose id is %s to %s (or %s) and restart, then run rotate_secret_key", ErrWrongKey, e.Stored, e.Configured, e.Stored, EnvOldKeys, EnvOldKeysFile)
}

func (e *WrongKeyError) Is(target error) bool { return target == ErrWrongKey }

type keyEntry struct {
	aead    cipher.AEAD
	id      [keyIDSize]byte
	idemKey []byte
}

type Keyring struct {
	keyEntry
	retired []keyEntry
}

func (k *Keyring) ID() string { return hex.EncodeToString(k.id[:]) }

func (k *Keyring) IDs() []string {
	out := []string{k.ID()}
	for _, e := range k.retired {
		out = append(out, hex.EncodeToString(e.id[:]))
	}
	return out
}

func (k *Keyring) ActiveID() []byte { return append([]byte(nil), k.id[:]...) }

func (k *Keyring) Fingerprint(plaintext string) string {
	return k.keyEntry.fingerprint(plaintext)
}

func (e *keyEntry) fingerprint(plaintext string) string {
	mac := hmac.New(sha256.New, e.idemKey)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

func (k *Keyring) Retired() []*Keyring {
	if k == nil {
		return nil
	}
	out := make([]*Keyring, len(k.retired))
	for i, e := range k.retired {
		out[i] = &Keyring{keyEntry: e}
	}
	return out
}

func newEntry(key []byte) (keyEntry, error) {
	if len(key) != KeySize {
		return keyEntry{}, fmt.Errorf("a secret key must be exactly %d bytes, got %d; generate one with: openssl rand -base64 32", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return keyEntry{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return keyEntry{}, err
	}
	derive := hmac.New(sha256.New, key)
	derive.Write([]byte("dolmen idempotency"))
	e := keyEntry{aead: aead, idemKey: derive.Sum(nil)}
	sum := sha256.Sum256(key)
	copy(e.id[:], sum[:keyIDSize])
	return e, nil
}

func New(key []byte, retired ...[]byte) (*Keyring, error) {
	active, err := newEntry(key)
	if err != nil {
		return nil, err
	}
	k := &Keyring{keyEntry: active}
	seen := map[[keyIDSize]byte]bool{active.id: true}
	for i, r := range retired {
		e, err := newEntry(r)
		if err != nil {
			return nil, fmt.Errorf("retired key %d: %w", i+1, err)
		}
		if seen[e.id] {
			continue
		}
		seen[e.id] = true
		k.retired = append(k.retired, e)
	}
	return k, nil
}

func KeyID(blob []byte) (string, bool) {
	if len(blob) < headerSize || blob[0] != version {
		return "", false
	}
	return hex.EncodeToString(blob[1 : 1+keyIDSize]), true
}

func (k *Keyring) entry(id []byte) *keyEntry {
	if bytes.Equal(id, k.id[:]) {
		return &k.keyEntry
	}
	for i := range k.retired {
		if bytes.Equal(id, k.retired[i].id[:]) {
			return &k.retired[i]
		}
	}
	return nil
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

func LoadOldKeys(getenv func(string) string) ([][]byte, error) {
	inline, file := getenv(EnvOldKeys), getenv(EnvOldKeysFile)
	var raw, name string
	switch {
	case inline != "" && file != "":
		return nil, fmt.Errorf("set only one of %s and %s", EnvOldKeys, EnvOldKeysFile)
	case inline != "":
		raw, name = inline, EnvOldKeys
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read the key file: %w", EnvOldKeysFile, err)
		}
		raw, name = string(b), EnvOldKeysFile
	default:
		return nil, nil
	}
	var keys [][]byte
	for i, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		k, err := ParseKey(f)
		if err != nil {
			return nil, fmt.Errorf("%s: key %d: %w", name, i+1, err)
		}
		keys = append(keys, k)
	}
	return keys, nil
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
	e := k.entry(blob[1 : 1+keyIDSize])
	if e == nil {
		return "", &WrongKeyError{Stored: hex.EncodeToString(blob[1 : 1+keyIDSize]), Configured: strings.Join(k.IDs(), ", ")}
	}
	plain, err := e.aead.Open(nil, blob[1+keyIDSize:headerSize], blob[headerSize:], blob[:1+keyIDSize])
	if err != nil {
		return "", ErrTampered
	}
	return string(plain), nil
}

type keyringKey struct{}

func WithKeyring(ctx context.Context, k *Keyring) context.Context {
	if k == nil {
		return ctx
	}
	return context.WithValue(ctx, keyringKey{}, k)
}

func KeyringFrom(ctx context.Context) *Keyring {
	k, _ := ctx.Value(keyringKey{}).(*Keyring)
	return k
}
