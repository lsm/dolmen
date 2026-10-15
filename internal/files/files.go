package files

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

const (
	DefaultMaxFileSize     int64 = 1 << 30
	DefaultMaxRequestFiles int64 = 2 << 30
	DefaultMaxPendingFiles int64 = 8 << 30
	MaxPartsPerRequest           = 100
	tmpDirName                   = ".files-tmp"
)

type Config struct {
	Root              string
	MaxFileSize       int64
	MaxNamespaceFiles int64
	MaxRequestFiles   int64
	MaxPendingFiles   int64
}

type Error struct {
	Status  int
	Code    derr.Code
	Message string
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return &derr.Error{Code: e.Code, Message: e.Message} }

func invalid(status int, format string, args ...any) *Error {
	return &Error{Status: status, Code: derr.InvalidRequest, Message: fmt.Sprintf(format, args...)}
}

type Store struct {
	cfg     Config
	mu      sync.Mutex
	pending int64
}

func New(cfg Config) *Store {
	if cfg.MaxFileSize <= 0 {
		cfg.MaxFileSize = DefaultMaxFileSize
	}
	if cfg.MaxRequestFiles <= 0 {
		cfg.MaxRequestFiles = DefaultMaxRequestFiles
	}
	if cfg.MaxPendingFiles <= 0 {
		cfg.MaxPendingFiles = DefaultMaxPendingFiles
	}
	return &Store{cfg: cfg}
}

func (s *Store) tmpDir() string { return filepath.Join(s.cfg.Root, tmpDirName) }

func (s *Store) reserve(n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending+n > s.cfg.MaxPendingFiles {
		return false
	}
	s.pending += n
	return true
}

func (s *Store) release(n int64) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	s.pending -= n
	s.mu.Unlock()
}

type part struct {
	name      string
	filename  string
	mediaType string
	tmp       string
	sum       string
	size      int64
	pending   int64
}

type Request struct {
	s      *Store
	mu     sync.Mutex
	parts  map[string]*part
	order  []string
	total  int64
	closed bool
}

func (s *Store) NewRequest() *Request {
	return &Request{s: s, parts: map[string]*part{}}
}

func (r *Request) Receive(name, filename, contentType string, body io.Reader) error {
	if name == "" {
		return invalid(http.StatusBadRequest, "a multipart file part has no form name; send each file as -F 'name=@path' and reference it as {\"part\": \"name\"}")
	}
	if name == "request" {
		return invalid(http.StatusBadRequest, "the form name \"request\" is reserved for the JSON request, which must be the first part; give each file part another name")
	}
	if _, dup := r.parts[name]; dup {
		return invalid(http.StatusBadRequest, "the multipart request has two parts named %q; give each file part its own name", name)
	}
	if len(r.parts) >= MaxPartsPerRequest {
		return invalid(http.StatusBadRequest, "a request may carry at most %d file parts; split the write into several requests", MaxPartsPerRequest)
	}
	if err := os.MkdirAll(r.s.tmpDir(), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(r.s.tmpDir(), "part-*")
	if err != nil {
		return err
	}
	p := &part{name: name, filename: BaseName(filename), tmp: f.Name()}
	r.parts[name] = p
	r.order = append(r.order, name)
	h := sha256.New()
	head := make([]byte, 0, 512)
	buf := make([]byte, 64<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if p.size+int64(n) > r.s.cfg.MaxFileSize {
				f.Close()
				return invalid(http.StatusRequestEntityTooLarge, "file part %q is larger than the server's -max-file-size of %d bytes; store a smaller file, or ask the operator to raise the limit", name, r.s.cfg.MaxFileSize)
			}
			if r.total+int64(n) > r.s.cfg.MaxRequestFiles {
				f.Close()
				return invalid(http.StatusRequestEntityTooLarge, "the file parts of this request total more than the server's -max-request-files-size of %d bytes; send the files in several requests", r.s.cfg.MaxRequestFiles)
			}
			if !r.s.reserve(int64(n)) {
				f.Close()
				return &Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable, Message: "the server is holding as many in-flight file bytes as -max-pending-files-size allows; retry the write shortly"}
			}
			p.pending += int64(n)
			if len(head) < cap(head) {
				head = append(head, buf[:min(n, cap(head)-len(head))]...)
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			h.Write(buf[:n])
			p.size += int64(n)
			r.total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	p.sum = hex.EncodeToString(h.Sum(nil))
	p.mediaType = InferMediaType(contentType, filename, head)
	return nil
}

func (r *Request) Describe(name string) (store.FileBlob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.parts[name]
	if !ok {
		return store.FileBlob{}, r.missingPart(name)
	}
	return p.blob(), nil
}

func (p *part) blob() store.FileBlob {
	return store.FileBlob{SHA256: p.sum, Size: p.size, MediaType: p.mediaType, Name: p.filename}
}

func (r *Request) missingPart(name string) error {
	if len(r.order) == 0 {
		return invalid(http.StatusBadRequest, "{\"part\": %q} names a multipart file part, but the request carries none; send the write as multipart/form-data with the file in a part named %s (curl -F 'request=<json>' -F '%s=@path')", name, name, name)
	}
	return invalid(http.StatusBadRequest, "{\"part\": %q} names no file part of this request; the request carries %s", name, quoteList(r.order))
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}

func (r *Request) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	var pending int64
	for _, p := range r.parts {
		if p.tmp != "" {
			os.Remove(p.tmp)
		}
		pending += p.pending
	}
	r.s.release(pending)
}
