package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const allowlistPath = "tools/nocomment/allowlist.txt"

func main() {
	check := flag.Bool("check", false, "exit 1 for comments outside the allowlist")
	write := flag.Bool("write", false, "strip comments in place")
	stats := flag.Bool("stats", false, "print per-file comment counts")
	flag.Parse()

	chosen := 0
	for _, on := range []bool{*check, *write, *stats} {
		if on {
			chosen++
		}
	}
	if chosen != 1 {
		fmt.Fprintln(os.Stderr, "nocomment: choose exactly one of -check, -write, -stats")
		os.Exit(2)
	}

	files, err := goFiles(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "nocomment:", err)
		os.Exit(2)
	}
	switch {
	case *stats:
		os.Exit(reportStats(files))
	case *write:
		os.Exit(stripFiles(files))
	default:
		os.Exit(checkAllowlist(files, allowlistPath))
	}
}

func goFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

func commentsIn(path string) ([]span, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return scan(src), nil
}

func loadAllowlist(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			allowed[line] = true
		}
	}
	return allowed, nil
}

func checkAllowlist(files []string, allowPath string) int {
	allowed, err := loadAllowlist(allowPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nocomment:", err)
		return 2
	}
	commented := map[string]bool{}
	outside := 0
	for _, path := range files {
		spans, err := commentsIn(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nocomment:", err)
			return 2
		}
		if len(spans) == 0 {
			continue
		}
		commented[path] = true
		if !allowed[path] {
			fmt.Println(path)
			outside++
		}
	}
	stale := 0
	for path := range allowed {
		if !commented[path] {
			fmt.Fprintf(os.Stderr, "nocomment: stale allowlist entry: %s\n", path)
			stale++
		}
	}
	fmt.Fprintf(os.Stderr, "nocomment: %d commented file(s) outside the allowlist, %d stale entry(ies)\n", outside, stale)
	if outside > 0 || stale > 0 {
		return 1
	}
	return 0
}

func reportStats(files []string) int {
	total, commented := 0, 0
	for _, path := range files {
		spans, err := commentsIn(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nocomment:", err)
			return 2
		}
		if len(spans) == 0 {
			continue
		}
		fmt.Printf("%s: %d\n", path, len(spans))
		total += len(spans)
		commented++
	}
	fmt.Fprintf(os.Stderr, "nocomment: %d comment(s) in %d file(s)\n", total, commented)
	return 0
}

func stripFiles(files []string) int {
	type rewrite struct {
		path string
		out  []byte
		mode os.FileMode
		info os.FileInfo
	}
	var pending []rewrite
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nocomment:", err)
			return 2
		}
		if info.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(os.Stderr, "nocomment: refusing to write through symlink: %s\n", path)
			return 2
		}
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nocomment:", err)
			return 2
		}
		out, err := strip(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nocomment: %s: %v\n", path, err)
			return 2
		}
		if !bytes.Equal(out, src) {
			pending = append(pending, rewrite{path: path, out: out, mode: info.Mode(), info: info})
		}
	}
	temps := make([]string, len(pending))
	for i, r := range pending {
		tmp, err := stageFile(r.path, r.out, r.mode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nocomment:", err)
			removeFiles(temps[:i])
			return 2
		}
		temps[i] = tmp
		if err := preserveOwner(tmp, r.info); err != nil {
			fmt.Fprintf(os.Stderr, "nocomment: %s: preserving owner: %v\n", r.path, err)
			removeFiles(temps[:i+1])
			return 2
		}
		if err := os.Chmod(tmp, r.mode); err != nil {
			fmt.Fprintf(os.Stderr, "nocomment: %s: restoring mode: %v\n", r.path, err)
			removeFiles(temps[:i+1])
			return 2
		}
	}
	for i, r := range pending {
		if err := os.Rename(temps[i], r.path); err != nil {
			fmt.Fprintln(os.Stderr, "nocomment:", err)
			if i > 0 {
				fmt.Fprintf(os.Stderr, "nocomment: %d file(s) were already rewritten before this failure\n", i)
			}
			removeFiles(temps[i:])
			return 2
		}
		fmt.Println(r.path)
	}
	return 0
}

func stageFile(path string, data []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nocomment-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		os.Remove(path)
	}
}
