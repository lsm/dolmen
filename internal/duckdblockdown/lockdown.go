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

func quoteSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func writeRC(home, dataDir, memoryLimit string, threads int) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SET allowed_directories = ['%s'];\n", quoteSQLLiteral(dataDir))
	b.WriteString("SET enable_external_access = false;\n")
	b.WriteString("SET autoinstall_known_extensions = false;\n")
	b.WriteString("SET autoload_known_extensions = false;\n")
	b.WriteString("SET allow_persistent_secrets = false;\n")
	if memoryLimit != "" {
		fmt.Fprintf(&b, "SET memory_limit = '%s';\n", quoteSQLLiteral(memoryLimit))
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

type settingValue struct {
	ExternalAccess string
	Lock           string
	Autoinstall    string
	Autoload       string
	Secrets        string
	AllowedDirs    string
}

func (l Locked) ReadSettings(ctx context.Context) (settingValue, error) {
	res, err := l.Run(ctx, l.settingsQuery())
	if err != nil {
		return settingValue{}, err
	}
	if res.ExitCode != 0 {
		return settingValue{}, fmt.Errorf("duckdblockdown: reading back the locked settings failed: %s", res.Combined())
	}
	row, ok := parseSettingRow(res.Stdout)
	if !ok {
		return settingValue{}, fmt.Errorf("duckdblockdown: the settings row did not parse, got %q", res.Stdout)
	}
	return row, nil
}

func parseSettingRow(out string) (settingValue, bool) {
	var v settingValue
	lines := strings.Split(out, "\n")
	header := -1
	var cols []int
	for i, line := range lines {
		fields := splitRow(line)
		if len(fields) < 6 {
			continue
		}
		if strings.TrimSpace(fields[0]) == "ext" {
			header = i
			for _, f := range fields {
				cols = append(cols, strings.Index(line, f))
			}
			break
		}
	}
	if header < 0 {
		return v, false
	}
	for _, line := range lines[header+1:] {
		fields := splitRow(line)
		if len(fields) != len(cols) {
			continue
		}
		bools := make([]string, 0, 5)
		for _, f := range fields[:5] {
			bools = append(bools, strings.TrimSpace(f))
		}
		isData := true
		for _, b := range bools {
			if b != "false" && b != "true" {
				isData = false
			}
		}
		if !isData {
			continue
		}
		v.ExternalAccess = bools[0]
		v.Lock = bools[1]
		v.Autoinstall = bools[2]
		v.Autoload = bools[3]
		v.Secrets = bools[4]
		v.AllowedDirs = strings.TrimSpace(fields[5])
		return v, true
	}
	return v, false
}

func splitRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "│") {
		return nil
	}
	parts := strings.Split(trimmed, "│")
	if len(parts) < 2 {
		return nil
	}
	return parts[1 : len(parts)-1]
}

func firstDataCell(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := splitRow(line)
		if len(fields) != 1 {
			continue
		}
		cell := strings.TrimSpace(fields[0])
		if cell == "" || cell == "varchar[]" || cell == "dirs" {
			continue
		}
		return cell
	}
	return ""
}

func (l Locked) VerifySettings(ctx context.Context) error {
	row, err := l.ReadSettings(ctx)
	if err != nil {
		return err
	}
	if row.ExternalAccess != "false" {
		return fmt.Errorf("duckdblockdown: enable_external_access is %q, want false; the sandbox is not in place", row.ExternalAccess)
	}
	if row.Lock != "true" {
		return fmt.Errorf("duckdblockdown: lock_configuration is %q, want true; a session could re-open what the process closed", row.Lock)
	}
	for name, got := range map[string]string{
		"autoinstall_known_extensions": row.Autoinstall,
		"autoload_known_extensions":    row.Autoload,
		"allow_persistent_secrets":     row.Secrets,
	} {
		if got != "false" {
			return fmt.Errorf("duckdblockdown: %s is %q, want false", name, got)
		}
	}
	if !strings.Contains(row.AllowedDirs, "data") {
		return fmt.Errorf("duckdblockdown: allowed_directories does not name the namespace data directory, got %q", row.AllowedDirs)
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
