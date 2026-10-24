package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"time"

	gb "github.com/lsm/dolmen/spike/graph-index"
	bi "github.com/lsm/dolmen/spike/graph-index/badger"
	si "github.com/lsm/dolmen/spike/graph-index/sqlite"
)

type timings struct {
	Case                              string
	Samples                           int
	OpsPerSecond, P50MS, P95MS, P99MS float64
	Found, Truncated                  int
	Scanned                           int64
	Checksum                          uint64
}
type report struct {
	Backend, Platform, Go, Directory                                   string
	Nodes, Edges, Procs                                                int
	LoadMS, ReopenMS, RebuildMS, CrashReopenMS                         float64
	PeakRSSBytes, HeapBytes, GoSysBytes, DiskBytes, AllocatedDiskBytes uint64
	Cases                                                              []timings
}

func fixture(n, e int) []gb.Edge {
	rng := rand.New(rand.NewPCG(718, 1))
	edges := make([]gb.Edge, e)
	for j := range edges {
		from, to := uint64(j%n+1), uint64((j+1)%n+1)
		if j >= n {
			from = uint64(rng.IntN(n) + 1)
			to = uint64(rng.IntN(n) + 1)
			if j%20 == 0 {
				from = 1
			}
		}
		edges[j] = gb.Edge{ID: uint64(j + 1), From: from, To: to, Kind: uint8(j%3 + 1)}
	}
	return edges
}
func open(backend, dir string) (gb.Index, error) {
	switch backend {
	case "sqlite":
		return si.Open(dir)
	case "badger":
		return bi.Open(dir)
	default:
		return nil, fmt.Errorf("-backend must be explicitly sqlite or badger")
	}
}
func load(db gb.Index, edges []gb.Edge) error {
	for start := 0; start < len(edges); start += 1000 {
		if err := db.Append(context.Background(), edges[start:min(start+1000, len(edges))]); err != nil {
			return err
		}
	}
	return nil
}
func visible(n uint64) bool { return (n/100)%4 != 3 }
func query(sample, n, hops int, path, seeds bool) gb.Query {
	start := uint64((sample*7919)%(n-8) + 1)
	for !visible(start) || !visible(start+4) || start%17 > 12 || start%17 == 0 {
		start = start%uint64(n-8) + 1
	}
	q := gb.Query{Seeds: []uint64{start}, Hops: hops, Nodes: 4096, Edges: 20000, Elapsed: 2 * time.Second, Visible: visible, EdgeVisible: func(e gb.Edge) bool { return e.ID%17 != 0 }}
	if path {
		q.Target = start + uint64(hops)
	}
	if seeds {
		q.Seeds = []uint64{start, start + 1, start + 2}
	}
	return q
}
func execute(db gb.Index, q gb.Query) (gb.Result, error) {
	ctx := context.Background()
	v, err := db.Snapshot(ctx)
	if err != nil {
		return gb.Result{}, err
	}
	r, err := gb.Traverse(ctx, v, q)
	closeErr := v.Close()
	if err != nil {
		return r, err
	}
	return r, closeErr
}
func measure(name string, samples int, fn func(int) (gb.Result, error)) (timings, error) {
	r := timings{Case: name, Samples: samples}
	latencies := make([]float64, 0, samples)
	var elapsed time.Duration
	for j := 0; j < samples; j++ {
		start := time.Now()
		got, err := fn(j)
		duration := time.Since(start)
		if err != nil {
			return r, err
		}
		elapsed += duration
		latencies = append(latencies, float64(duration)/float64(time.Millisecond))
		if len(got.Path) > 0 {
			r.Found++
		}
		if got.Truncated {
			r.Truncated++
		}
		r.Scanned += int64(got.Scanned)
		for _, n := range got.Nodes {
			r.Checksum = r.Checksum*31 + n
		}
		for _, id := range got.EdgeIDs {
			r.Checksum = r.Checksum*31 + id
		}
		for _, n := range got.Path {
			r.Checksum = r.Checksum*31 + n
		}
	}
	slices.Sort(latencies)
	r.OpsPerSecond = float64(samples) / elapsed.Seconds()
	r.P50MS = latencies[(samples*50+99)/100-1]
	r.P95MS = latencies[(samples*95+99)/100-1]
	r.P99MS = latencies[(samples*99+99)/100-1]
	return r, nil
}
func disk(dir string) (uint64, uint64, error) {
	var apparent, allocated uint64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		apparent += uint64(info.Size())
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			allocated += uint64(stat.Blocks) * 512
		}
		return nil
	})
	return apparent, allocated, err
}
func run(backend, dir string, n, e, samples int, crash bool) (report, error) {
	r := report{Backend: backend, Platform: runtime.GOOS + "/" + runtime.GOARCH, Go: runtime.Version(), Nodes: n, Edges: e, Procs: runtime.GOMAXPROCS(0)}
	if n < 10 || e < n || samples < 100 || samples%100 != 0 {
		return r, fmt.Errorf("require nodes >=10, edges >= nodes, samples >=100 in multiples of 100")
	}
	edges := fixture(n, e)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return r, err
	}
	if crash {
		db, err := open(backend, dir)
		if err != nil {
			return r, err
		}
		if err := load(db, edges); err != nil {
			return r, err
		}
		os.Exit(0)
	}
	root, err := os.MkdirTemp(dir, backend+"-")
	if err != nil {
		return r, err
	}
	r.Directory = root
	start := time.Now()
	db, err := open(backend, filepath.Join(root, "index"))
	if err != nil {
		return r, err
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()
	if err := load(db, edges); err != nil {
		return r, err
	}
	r.LoadMS = float64(time.Since(start)) / float64(time.Millisecond)
	for j := 0; j < 20; j++ {
		if _, err := execute(db, query(j, n, 2, false, false)); err != nil {
			return r, err
		}
	}
	add := func(name string, fn func(int) (gb.Result, error)) error {
		m, err := measure(name, samples, fn)
		if err == nil {
			r.Cases = append(r.Cases, m)
		}
		return err
	}
	if err := add("neighbors", func(j int) (gb.Result, error) { return execute(db, query(j, n, 1, false, false)) }); err != nil {
		return r, err
	}
	if err := add("hub_neighbors", func(j int) (gb.Result, error) {
		q := query(j, n, 1, false, false)
		q.Seeds = []uint64{1}
		return execute(db, q)
	}); err != nil {
		return r, err
	}
	for hops := 1; hops <= 4; hops++ {
		for _, path := range []bool{false, true} {
			name := fmt.Sprintf("expand_%d", hops)
			if path {
				name = fmt.Sprintf("path_%d", hops)
			}
			if err := add(name, func(j int) (gb.Result, error) { return execute(db, query(j, n, hops, path, false)) }); err != nil {
				return r, err
			}
		}
	}
	if err := add("semantic_seeds_expand_3", func(j int) (gb.Result, error) { return execute(db, query(j, n, 3, false, true)) }); err != nil {
		return r, err
	}
	next := uint64(e + 1)
	readLat, writeLat := []float64{}, []float64{}
	readSummary := timings{Case: "mixed_95_reads", Samples: samples * 19 / 10}
	writeSummary := timings{Case: "mixed_5_writes", Samples: samples / 10}
	for j := 0; j < samples*2; j++ {
		start := time.Now()
		if j%20 == 19 {
			err = db.Append(context.Background(), []gb.Edge{{ID: next, From: uint64(j%n + 1), To: uint64((j+3)%n + 1), Kind: 1}})
			next++
			writeLat = append(writeLat, float64(time.Since(start))/float64(time.Millisecond))
		} else {
			var result gb.Result
			result, err = execute(db, query(j, n, 3, false, false))
			readLat = append(readLat, float64(time.Since(start))/float64(time.Millisecond))
			readSummary.Scanned += int64(result.Scanned)
			if result.Truncated {
				readSummary.Truncated++
			}
			for _, id := range result.EdgeIDs {
				readSummary.Checksum = readSummary.Checksum*31 + id
			}
		}
		if err != nil {
			return r, err
		}
	}
	for _, entry := range []struct {
		s   *timings
		lat []float64
	}{{&readSummary, readLat}, {&writeSummary, writeLat}} {
		var sum float64
		for _, v := range entry.lat {
			sum += v
		}
		slices.Sort(entry.lat)
		count := len(entry.lat)
		entry.s.OpsPerSecond = float64(count) * 1000 / sum
		entry.s.P50MS = entry.lat[(count*50+99)/100-1]
		entry.s.P95MS = entry.lat[(count*95+99)/100-1]
		entry.s.P99MS = entry.lat[(count*99+99)/100-1]
		r.Cases = append(r.Cases, *entry.s)
	}
	r.DiskBytes, r.AllocatedDiskBytes, err = disk(filepath.Join(root, "index"))
	if err != nil {
		return r, err
	}
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	r.HeapBytes = memory.HeapAlloc
	r.GoSysBytes = memory.Sys
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return r, err
	}
	r.PeakRSSBytes = uint64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		r.PeakRSSBytes *= 1024
	}
	if err := db.Close(); err != nil {
		return r, err
	}
	closed = true
	start = time.Now()
	db, err = open(backend, filepath.Join(root, "index"))
	if err != nil {
		return r, err
	}
	closed = false
	if _, err := execute(db, query(0, n, 3, false, false)); err != nil {
		return r, err
	}
	r.ReopenMS = float64(time.Since(start)) / float64(time.Millisecond)
	if err := db.Close(); err != nil {
		return r, err
	}
	closed = true
	start = time.Now()
	rebuilt, err := open(backend, filepath.Join(root, "rebuild"))
	if err != nil {
		return r, err
	}
	if err := load(rebuilt, edges); err != nil {
		rebuilt.Close()
		return r, err
	}
	if err := rebuilt.Close(); err != nil {
		return r, err
	}
	r.RebuildMS = float64(time.Since(start)) / float64(time.Millisecond)
	binary, err := os.Executable()
	if err != nil {
		return r, err
	}
	crashDir := filepath.Join(root, "crash")
	child := exec.Command(binary, "-backend", backend, "-data", crashDir, "-nodes", fmt.Sprint(n), "-edges", fmt.Sprint(e), "-samples", fmt.Sprint(samples), "-crash")
	if output, err := child.CombinedOutput(); err != nil {
		return r, fmt.Errorf("abrupt writer: %w %s", err, output)
	}
	start = time.Now()
	recovered, err := open(backend, crashDir)
	if err != nil {
		return r, err
	}
	got, err := execute(recovered, query(0, n, 1, false, false))
	if err != nil {
		recovered.Close()
		return r, err
	}
	r.CrashReopenMS = float64(time.Since(start)) / float64(time.Millisecond)
	expected, err := open(backend, filepath.Join(root, "rebuild"))
	if err != nil {
		recovered.Close()
		return r, err
	}
	want, readErr := execute(expected, query(0, n, 1, false, false))
	expected.Close()
	recovered.Close()
	if readErr != nil {
		return r, readErr
	}
	if !slices.Equal(got.EdgeIDs, want.EdgeIDs) {
		return r, fmt.Errorf("acknowledged edges missing after abrupt exit")
	}
	return r, nil
}
func main() {
	backend := flag.String("backend", "", "explicit sqlite or badger")
	dir := flag.String("data", "", "owned synthetic data parent")
	nodes := flag.Int("nodes", 10000, "synthetic nodes")
	edges := flag.Int("edges", 100000, "synthetic edges")
	samples := flag.Int("samples", 100, "samples per case")
	crash := flag.Bool("crash", false, "exit after durable load without Close")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-data is required")
		os.Exit(1)
	}
	r, err := run(*backend, *dir, *nodes, *edges, *samples, *crash)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
