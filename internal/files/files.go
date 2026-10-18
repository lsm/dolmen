package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

const (
	DefaultMaxFileSize     int64 = 1 << 30
	DefaultMaxRequestFiles int64 = 2 << 30
	DefaultMaxPendingFiles int64 = 8 << 30
	MaxPartsPerRequest           = 100
	tmpDirName                   = ".files-tmp"
	NamespaceDirSuffix           = ".files"
)

type Config struct {
	Root              string
	MaxFileSize       int64
	MaxNamespaceFiles int64
	MaxRequestFiles   int64
	MaxPendingFiles   int64
	FetchHosts        []string
	FetchTimeout      time.Duration
	FetchWording      *FetchWording
	Blobs             BlobStore
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
	cfg      Config
	mu       sync.Mutex
	pending  int64
	totals   map[string]int64
	pins     map[string]map[string]int
	attaches map[string]map[string]int
	live     map[string]bool
	fetcher  *Fetcher
	blobs    BlobStore
	drops    map[string]int
	placing  map[[2]string]*hashLock
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
	fetcher := NewFetcher(cfg.FetchHosts, cfg.FetchTimeout)
	if cfg.FetchWording != nil {
		fetcher.Worded(*cfg.FetchWording)
	}
	if cfg.Blobs == nil {
		cfg.Blobs = NewLocal(cfg.Root)
	}
	return &Store{cfg: cfg, blobs: cfg.Blobs, drops: map[string]int{}, placing: map[[2]string]*hashLock{}, totals: map[string]int64{}, pins: map[string]map[string]int{}, attaches: map[string]map[string]int{}, live: map[string]bool{}, fetcher: fetcher}
}

func (s *Store) Fetches() bool { return s.fetcher.Enabled() }

func (s *Store) Remote() bool {
	sh, ok := s.blobs.(interface{ Shared() bool })
	return ok && sh.Shared()
}

func (s *Store) dropping(ns string) error {
	if s.drops[ns] > 0 {
		return &Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable, Message: fmt.Sprintf("namespace %s is being dropped and its files deleted; retry the write once the drop finishes", ns)}
	}
	return nil
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
	tmpName   string
	placed    map[string]bool
}

type Request struct {
	s       *Store
	mu      sync.Mutex
	parts   map[string]*part
	order   []string
	total   int64
	pinned  [][2]string
	fetched map[string]string
	created []created
	closed  bool
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
		return storageErr(err)
	}
	r.s.mu.Lock()
	f, err := os.CreateTemp(r.s.tmpDir(), "part-*")
	if err == nil {
		r.s.live[f.Name()] = true
	}
	r.s.mu.Unlock()
	if err != nil {
		return storageErr(err)
	}
	p := &part{name: name, filename: BaseName(filename), tmp: f.Name(), tmpName: f.Name(), placed: map[string]bool{}}
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
				return storageErr(err)
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
		return storageErr(err)
	}
	if err := f.Close(); err != nil {
		return storageErr(err)
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
	r.s.mu.Lock()
	for _, p := range r.parts {
		delete(r.s.live, p.tmpName)
	}
	for _, pin := range r.pinned {
		r.s.unpin(pin[0], pin[1])
	}
	for _, c := range r.created {
		r.s.forget(c)
	}
	r.s.mu.Unlock()
}

func ValidHash(sum string) bool {
	if len(sum) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil && strings.ToLower(sum) == sum
}

func (s *Store) Open(ctx context.Context, ns, sum string) (Blob, error) {
	if !ValidHash(sum) {
		return nil, fs.ErrNotExist
	}
	return s.blobs.Open(ctx, ns, sum)
}

func (s *Store) namespaceTotal(ns string) (int64, error) {
	if t, ok := s.totals[ns]; ok {
		return t, nil
	}
	var total int64
	err := s.blobs.List(context.Background(), ns, func(_ string, info BlobInfo) error {
		total += info.Size
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.totals[ns] = total
	return total, nil
}

func (s *Store) Pinned(ns, sum string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[ns][sum] > 0
}

func (s *Store) pin(ns, sum string) {
	if s.pins[ns] == nil {
		s.pins[ns] = map[string]int{}
	}
	s.pins[ns][sum]++
}

func (s *Store) unpin(ns, sum string) {
	m := s.pins[ns]
	if m[sum] <= 1 {
		delete(m, sum)
		if len(m) == 0 {
			delete(s.pins, ns)
		}
		return
	}
	m[sum]--
}

func (r *Request) Attach(ctx context.Context, ns, name string) (store.FileBlob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.parts[name]
	if !ok {
		return store.FileBlob{}, r.missingPart(name)
	}
	if p.placed[ns] {
		return p.blob(), nil
	}
	if p.tmp == "" {
		return store.FileBlob{}, fmt.Errorf("file part %q was already stored in another namespace", name)
	}
	s := r.s
	if s.cfg.MaxNamespaceFiles > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.dropping(ns); err != nil {
			return store.FileBlob{}, err
		}
		if err := r.placeCapped(ctx, ns, p); err != nil {
			return store.FileBlob{}, err
		}
	} else {
		s.mu.Lock()
		if err := s.dropping(ns); err != nil {
			s.mu.Unlock()
			return store.FileBlob{}, err
		}
		s.pin(ns, p.sum)
		s.mu.Unlock()
		err := r.place(ctx, ns, p)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.unpin(ns, p.sum)
		if err != nil {
			return store.FileBlob{}, err
		}
	}
	p.tmp = ""
	s.pending -= p.pending
	p.pending = 0
	s.pin(ns, p.sum)
	r.pinned = append(r.pinned, [2]string{ns, p.sum})
	p.placed[ns] = true
	return p.blob(), nil
}

type hashLock struct {
	mu   sync.Mutex
	refs int
}

func (s *Store) lockHash(ns, sum string) func() {
	key := [2]string{ns, sum}
	s.mu.Lock()
	l := s.placing[key]
	if l == nil {
		l = &hashLock{}
		s.placing[key] = l
	}
	l.refs++
	s.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.placing, key)
		}
		s.mu.Unlock()
	}
}

func (r *Request) place(ctx context.Context, ns string, p *part) error {
	s := r.s
	defer s.lockHash(ns, p.sum)()
	info, err := s.blobs.Stat(ctx, ns, p.sum)
	exists := err == nil && info.Size == p.size
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if exists {
		if err := os.Remove(p.tmp); err != nil {
			return err
		}
	} else if err := s.blobs.Put(ctx, ns, p.sum, p.tmp); err != nil {
		return storageErr(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.attaches[ns][p.sum]; ok {
		s.attaches[ns][p.sum] = n + 1
	} else if !exists {
		if s.attaches[ns] == nil {
			s.attaches[ns] = map[string]int{}
		}
		s.attaches[ns][p.sum] = 1
		r.created = append(r.created, created{ns: ns, sum: p.sum, size: p.size})
	}
	return nil
}

func (r *Request) placeCapped(ctx context.Context, ns string, p *part) error {
	s := r.s
	info, err := s.blobs.Stat(ctx, ns, p.sum)
	switch {
	case err == nil && info.Size == p.size:
		if err := os.Remove(p.tmp); err != nil {
			return err
		}
		if n, ok := s.attaches[ns][p.sum]; ok {
			s.attaches[ns][p.sum] = n + 1
		}
	case err == nil || errors.Is(err, fs.ErrNotExist):
		total, terr := s.namespaceTotal(ns)
		if terr != nil {
			return terr
		}
		if err == nil {
			total -= info.Size
		}
		if total+p.size > s.cfg.MaxNamespaceFiles {
			return &Error{Status: http.StatusInsufficientStorage, Code: derr.Internal, Message: fmt.Sprintf("namespace %s holds %d bytes of files and this %d-byte file would pass its -max-namespace-files-size of %d bytes, so nothing was written; clear file fields or delete rows that hold files and run vacuum, or have the operator raise the limit", ns, total, p.size, s.cfg.MaxNamespaceFiles)}
		}
		if err := s.blobs.Put(ctx, ns, p.sum, p.tmp); err != nil {
			return storageErr(err)
		}
		s.totals[ns] = total + p.size
		if s.attaches[ns] == nil {
			s.attaches[ns] = map[string]int{}
		}
		s.attaches[ns][p.sum] = 1
		r.created = append(r.created, created{ns: ns, sum: p.sum, size: p.size})
	default:
		return err
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

var _ store.FileSource = (*Request)(nil)

type created struct {
	ns   string
	sum  string
	size int64
}

func (r *Request) Discard() {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()
	shared := s.Remote()
	for _, c := range r.created {
		if !shared && s.attaches[c.ns][c.sum] == 1 && s.pins[c.ns][c.sum] <= 1 {
			if t, ok := s.totals[c.ns]; s.blobs.Delete(context.Background(), c.ns, c.sum) == nil && ok {
				s.totals[c.ns] = t - c.size
			}
		}
		s.forget(c)
	}
	r.created = nil
}

func (s *Store) forget(c created) {
	delete(s.attaches[c.ns], c.sum)
	if len(s.attaches[c.ns]) == 0 {
		delete(s.attaches, c.ns)
	}
}

func (s *Store) Sweep(ctx context.Context, ns string, referenced map[string]bool) (store.FileSweep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out store.FileSweep
	err := s.blobs.List(ctx, ns, func(sum string, info BlobInfo) error {
		if referenced[sum] || s.pins[ns][sum] > 0 {
			out.Bytes += info.Size
			return nil
		}
		if err := s.blobs.Delete(ctx, ns, sum); err != nil {
			return err
		}
		if t, ok := s.totals[ns]; ok {
			s.totals[ns] = t - info.Size
		}
		out.Removed++
		out.RemovedBytes += info.Size
		return nil
	})
	if err != nil {
		return store.FileSweep{}, err
	}
	s.totals[ns] = out.Bytes
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return store.FileSweep{}, err
	}
	for _, e := range entries {
		if path := filepath.Join(s.tmpDir(), e.Name()); !s.live[path] {
			os.Remove(path)
		}
	}
	return out, nil
}

var _ store.FileSweeper = (*Store)(nil)

func storageErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return &Error{Status: http.StatusInsufficientStorage, Code: derr.Internal, Message: "the server's disk is full, so nothing was written; have the operator free disk space"}
	}
	return err
}

func (r *Request) Pin(ns, sum string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.s
	if !ValidHash(sum) {
		return invalid(http.StatusBadRequest, "the file to copy has no valid hash")
	}
	s.mu.Lock()
	if err := s.dropping(ns); err != nil {
		s.mu.Unlock()
		return err
	}
	s.pin(ns, sum)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), storeCallTimeout)
	defer cancel()
	_, err := s.blobs.Stat(ctx, ns, sum)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.unpin(ns, sum)
		if errors.Is(err, fs.ErrNotExist) {
			return invalid(http.StatusNotFound, "the file to copy was removed while this write ran; read the source row again and retry")
		}
		return err
	}
	if n, ok := s.attaches[ns][sum]; ok {
		s.attaches[ns][sum] = n + 1
	}
	r.pinned = append(r.pinned, [2]string{ns, sum})
	return nil
}

const storeCallTimeout = 30 * time.Second
