package files

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const signingKeyFile = "files.key"

type Signer struct {
	path string
	mu   sync.Mutex
	key  []byte
}

func (s *Store) Signer() *Signer { return s.signer }

func (g *Signer) loadKey(create bool) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key != nil {
		return g.key, nil
	}
	b, err := os.ReadFile(g.path)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, nil
		}
		b, err = g.createKey()
	}
	if err != nil {
		return nil, err
	}
	if len(b) != sha256.Size {
		return nil, fmt.Errorf("%s holds %d bytes, not a %d-byte download signing key; delete it and the server makes a new one, which invalidates every download URL issued before", filepath.Base(g.path), len(b), sha256.Size)
	}
	g.key = b
	return b, nil
}

func (g *Signer) createKey() ([]byte, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(g.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return os.ReadFile(g.path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

func signedMessage(ns, table string, id int64, field, sum string, exp int64) []byte {
	var b []byte
	for _, part := range []string{ns, table, strconv.FormatInt(id, 10), field, sum, strconv.FormatInt(exp, 10)} {
		b = strconv.AppendInt(b, int64(len(part)), 10)
		b = append(b, ':')
		b = append(b, part...)
	}
	return b
}

func mac(key []byte, ns, table string, id int64, field, sum string, exp int64) string {
	m := hmac.New(sha256.New, key)
	m.Write(signedMessage(ns, table, id, field, sum, exp))
	return hex.EncodeToString(m.Sum(nil))
}

func (g *Signer) Sign(ns, table string, id int64, field, sum string, exp int64) (string, error) {
	key, err := g.loadKey(true)
	if err != nil {
		return "", err
	}
	return mac(key, ns, table, id, field, sum, exp), nil
}

func (g *Signer) Valid(ns, table string, id int64, field, sum string, exp int64, sig string) (bool, error) {
	key, err := g.loadKey(false)
	if err != nil || key == nil {
		return false, err
	}
	return hmac.Equal([]byte(mac(key, ns, table, id, field, sum, exp)), []byte(sig)), nil
}
