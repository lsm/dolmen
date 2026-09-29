package duckdblockdown

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Options struct {
	Bin         string
	Namespace   string
	MemoryLimit string
	Threads     int
}

type Locked struct {
	Bin     string
	DataDir string
	Home    string
	RCD     string
}

func LockdownDir(namespace string) string {
	return filepath.Join(namespace, "data")
}

func writeRC(home, dataDir, memoryLimit string, threads int) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SET allowed_directories = ['%s'];\n", dataDir)
	b.WriteString("SET enable_external_access = false;\n")
	b.WriteString("SET autoinstall_known_extensions = false;\n")
	b.WriteString("SET autoload_known_extensions = false;\n")
	b.WriteString("SET allow_persistent_secrets = false;\n")
	if memoryLimit != "" {
		fmt.Fprintf(&b, "SET memory_limit = '%s';\n", memoryLimit)
	}
	if threads > 0 {
		fmt.Fprintf(&b, "SET threads = %d;\n", threads)
	}
	b.WriteString("SET lock_configuration = true;\n")
	rc := filepath.Join(home, ".duckdbrc")
	if err := os.WriteFile(rc, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return nil
}

func Lock(opts Options) (Locked, error) {
	if opts.Bin == "" {
		return Locked{}, errors.New("duckdblockdown: a duckdb binary path is required")
	}
	if opts.Namespace == "" {
		return Locked{}, errors.New("duckdblockdown: a namespace directory is required")
	}
	data := LockdownDir(opts.Namespace)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return Locked{}, err
	}
	home, err := os.MkdirTemp("", "dolmen-lakehouse-spike-")
	if err != nil {
		return Locked{}, err
	}
	abs, err := filepath.Abs(data)
	if err != nil {
		os.RemoveAll(home)
		return Locked{}, err
	}
	if err := writeRC(home, abs, opts.MemoryLimit, opts.Threads); err != nil {
		os.RemoveAll(home)
		return Locked{}, err
	}
	return Locked{Bin: opts.Bin, DataDir: abs, Home: home, RCD: filepath.Join(home, ".duckdbrc")}, nil
}

func (l Locked) Close() error {
	if l.Home == "" {
		return nil
	}
	return os.RemoveAll(l.Home)
}

func (l Locked) settingsQuery() string {
	return "SELECT current_setting('enable_external_access') AS ext, " +
		"current_setting('lock_configuration') AS lock, " +
		"current_setting('autoinstall_known_extensions') AS autoinstall, " +
		"current_setting('autoload_known_extensions') AS autoload, " +
		"current_setting('allow_persistent_secrets') AS secrets, " +
		"current_setting('allowed_directories') AS dirs"
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r Result) Combined() string {
	return r.Stdout + r.Stderr
}

func (r Result) ContainsAny(substrs ...string) bool {
	c := r.Combined()
	for _, s := range substrs {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

func (l Locked) Run(ctx context.Context, sql string, args ...string) (Result, error) {
	if l.Bin == "" {
		return Result{}, errors.New("duckdblockdown: the binary path is not set")
	}
	full := append([]string{"-no-stdin"}, args...)
	if sql != "" {
		full = append(full, "-c", sql)
	}
	cmd := exec.CommandContext(ctx, l.Bin, full...)
	cmd.Env = append(os.Environ(), "HOME="+l.Home)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if runErr != nil {
		return res, runErr
	}
	return res, nil
}

func (l Locked) RunStdinMode(ctx context.Context, sql string, args ...string) (Result, error) {
	if l.Bin == "" {
		return Result{}, errors.New("duckdblockdown: the binary path is not set")
	}
	full := append([]string{}, args...)
	cmd := exec.CommandContext(ctx, l.Bin, full...)
	cmd.Env = append(os.Environ(), "HOME="+l.Home)
	in, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	_, writeErr := in.Write([]byte(sql + "\n"))
	closeErr := in.Close()
	waitErr := cmd.Wait()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if writeErr != nil {
		return res, writeErr
	}
	if closeErr != nil {
		return res, closeErr
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}

func (l Locked) RunStdin(ctx context.Context, sql string) (Result, error) {
	return l.RunStdinMode(ctx, sql)
}
func (l Locked) VerifySettings(ctx context.Context) error {
	res, err := l.Run(ctx, l.settingsQuery())
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("duckdblockdown: reading back the locked settings failed: %s", res.Combined())
	}
	out := res.Stdout
	for _, want := range []string{"false", "true"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("duckdblockdown: the locked settings did not take effect, got %q", out)
		}
	}
	return nil
}

func HasDotCommandLine(sql string) bool {
	sc := bufio.NewScanner(strings.NewReader(sql))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, ".") {
			return true
		}
	}
	return sc.Err() != nil
}
