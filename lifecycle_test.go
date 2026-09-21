package dolmen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

func TestChangeRetentionOptionPassesThrough(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(time.Hour))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if st.changeRetention != time.Hour {
		t.Fatalf("retention option must be recorded, got %v", st.changeRetention)
	}
}

func TestOpenRejectsInvalidOptions(t *testing.T) {
	if _, err := Open(t.TempDir(), nil); err == nil {
		t.Fatal("a nil option must be rejected")
	}
	if _, err := Open(t.TempDir(), WithEmbedding(nil)); err == nil {
		t.Fatal("a nil embedding provider must be rejected")
	}
	if _, err := Open(t.TempDir(), WithChangeRetention(-1)); err == nil {
		t.Fatal("a negative change retention must be rejected")
	}
	if _, err := Open(t.TempDir(), WithEngine("banana")); err == nil {
		t.Fatal("an unknown engine name must be rejected")
	}
	if _, err := Open(t.TempDir(), WithEngine("postgres")); err == nil {
		t.Fatal("the postgres engine without a connection must be rejected")
	}
	opener := func(context.Context, time.Duration) (store.Engine, error) { return nil, errors.New("unused") }
	if _, err := Open(t.TempDir(), WithEngineOpener("postgres", "", opener)); err == nil {
		t.Fatal("an engine opener without an owner key must be rejected")
	}
	if _, err := Open(t.TempDir(), WithEngineOpener("postgres", "key", nil)); err == nil {
		t.Fatal("an owner key without an opener must be rejected")
	}
}

func TestEngineOptionAcceptsSQLite(t *testing.T) {
	st, err := Open(t.TempDir(), WithEngine("sqlite"))
	if err != nil {
		t.Fatalf("open with the sqlite engine: %v", err)
	}
	defer st.Close()
}

func TestOpenDuplicateDirectoryIsRejected(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()
	second, err := Open(dir)
	if err == nil {
		second.Close()
		t.Fatal("a duplicate live open of one data directory must be rejected")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate open must carry the conflict category, got %v", err)
	}
}

func TestOpenRejectsPathWhereDirectoryIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := Open(file)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a file in place of the data directory must be invalid_request, got %v", err)
	}
	if got := errors.Unwrap(err); got == nil {
		t.Fatalf("the filesystem cause must be preserved, got %v", err)
	}
}

func TestOpenRejectsPathWhereParentIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := Open(filepath.Join(file, "data"))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a file in place of a parent directory must be invalid_request, got %v", err)
	}
}

func TestOpenDuplicateThroughSymlinkAliasIsRejected(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	first, err := Open(real)
	if err != nil {
		t.Fatalf("open real: %v", err)
	}
	defer first.Close()
	if _, err := Open(alias); err == nil {
		t.Fatal("opening through a symlink alias of a live directory must be rejected")
	}
}

func TestOpenDeeperSymlinkIsDistinct(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(a, "d"), 0o700); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(b, "d"), 0o700); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	first, err := Open(filepath.Join(a, "d"))
	if err != nil {
		t.Fatalf("open a/d: %v", err)
	}
	defer first.Close()
	second, err := Open(filepath.Join(b, "d"))
	if err != nil {
		t.Fatalf("distinct directories must open independently, got %v", err)
	}
	defer second.Close()
}

func TestReopenAfterCloseSucceeds(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after close must succeed, got %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("repeated close must return the same completion result, got %v", err)
	}
}

func TestFailedConstructionReleasesOwnership(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if _, err := Open(blocker); err == nil {
		t.Fatal("opening a file as the data directory must fail")
	}
	ownersMu.Lock()
	leaked := len(owners)
	ownersMu.Unlock()
	if leaked != 0 {
		t.Fatalf("failed construction must release directory ownership, %d entries remain", leaked)
	}
}

func TestCloseReleasesOwnership(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ownersMu.Lock()
	leaked := len(owners)
	ownersMu.Unlock()
	if leaked != 0 {
		t.Fatalf("close must release directory ownership, %d entries remain", leaked)
	}
}

func TestOpenResolvesSymlinkedAncestorOfMissingDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	first, err := Open(filepath.Join(root, "link", "data"))
	if err != nil {
		t.Fatalf("open through a symlink to a not-yet-existing directory: %v", err)
	}
	defer first.Close()
	if _, err := Open(filepath.Join(target, "data")); err == nil {
		t.Fatal("opening the resolved path of a live directory must be rejected")
	}
}

func TestConcurrentCloseReturnsSameResult(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

func TestOpenDotDotThroughSymlinkSharesOwnership(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(target, "data"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	first, err := Open(root + string(filepath.Separator) + "link" + string(filepath.Separator) + ".." + string(filepath.Separator) + "data")
	if err != nil {
		t.Fatalf("open through link/..: %v", err)
	}
	defer first.Close()
	if first.dir != filepath.Join(root, "data") {
		t.Fatalf("link/../data must canonicalize physically (link resolves, then .. pops), got %s", first.dir)
	}
	if _, err := Open(target + string(filepath.Separator) + ".." + string(filepath.Separator) + "data"); err == nil {
		t.Fatal("the same physical directory reached via target/../data must still be single-owner")
	}
}

func TestOpenThroughDanglingSymlinkCreatesAtTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	st, err := Open(filepath.Join(root, "link", "data"))
	if err != nil {
		t.Fatalf("open through a dangling symlink: %v", err)
	}
	defer st.Close()
	if fi, err := os.Stat(filepath.Join(target, "data")); err != nil || !fi.IsDir() {
		t.Fatalf("the data directory must materialize at the link target, got %v (%v)", fi, err)
	}
}

func TestOpenResolvesRelativeSymlinkTargetInPlace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(root, "app", "data")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	st, err := Open(root + string(filepath.Separator) + "app" + string(filepath.Separator) + "data")
	if err != nil {
		t.Fatalf("open through a relative symlink: %v", err)
	}
	defer st.Close()
	if st.dir != filepath.Join(root, "app", "real") {
		t.Fatalf("a relative target anchors beside the link, got %s", st.dir)
	}
}

func TestOpenRejectsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "b"), filepath.Join(root, "a")); err != nil {
		t.Fatalf("symlink a: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatalf("symlink b: %v", err)
	}
	if _, err := Open(filepath.Join(root, "a")); err == nil {
		t.Fatal("a symlink cycle must be rejected, not silently mis-anchored")
	}
}

func TestOpenRejectsEmptyDirectory(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("an empty data directory must be rejected")
	}
}

type nilAbleProvider struct{ identity string }

func (p *nilAbleProvider) Identity() string { return p.identity }

func (p *nilAbleProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, nil
}

func (p *nilAbleProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func TestOpenRejectsTypedNilProvider(t *testing.T) {
	if _, err := Open(t.TempDir(), WithEmbedding((*nilAbleProvider)(nil))); err == nil {
		t.Fatal("a typed-nil provider must be rejected at open, not panic at first use")
	}
	live := &nilAbleProvider{identity: "x"}
	st, err := Open(t.TempDir(), WithEmbedding(live))
	if err != nil {
		t.Fatalf("a live pointer provider must open: %v", err)
	}
	defer st.Close()
}

func TestOpenResolvesRelativeTargetThroughSymlinkAndDotDot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o700); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "other"), 0o700); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "b", "hop")); err != nil {
		t.Fatalf("symlink hop: %v", err)
	}
	if err := os.Symlink("b/hop/../data", filepath.Join(root, "alias")); err != nil {
		t.Fatalf("symlink alias: %v", err)
	}
	st, err := Open(filepath.Join(root, "alias"))
	if err != nil {
		t.Fatalf("open alias: %v", err)
	}
	defer st.Close()
	want := filepath.Join(root, "data")
	if st.dir != want {
		t.Fatalf("a relative target must traverse its own symlinks before .. pops: want %s, got %s", want, st.dir)
	}
}
