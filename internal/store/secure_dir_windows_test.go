package store

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func processUserSID(t *testing.T) string {
	t.Helper()
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatalf("open the process token: %v", err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		t.Fatalf("read the process user: %v", err)
	}
	return tu.User.Sid.String()
}

func setDACL(t *testing.T, path, dacl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		t.Fatalf("build the descriptor %q: %v", dacl, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("read the dacl of %q: %v", dacl, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatalf("set the dacl on %s: %v", path, err)
	}
}

type daclEntry struct {
	kind   string
	flags  string
	rights string
	sid    string
}

func readDACL(t *testing.T, path string) (protected bool, entries []daclEntry) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the security descriptor of %s: %v", path, err)
	}
	sddl := sd.String()
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return false, nil
	}
	section := sddl[start:]
	if end := strings.Index(section[1:], "S:"); end >= 0 {
		section = section[:end+1]
	}
	protected = strings.HasPrefix(section, "D:P")
	for rest := section; ; {
		open := strings.Index(rest, "(")
		if open < 0 {
			return protected, entries
		}
		closing := strings.Index(rest[open:], ")")
		if closing < 0 {
			t.Fatalf("the dacl of %s is truncated: %q", path, sddl)
		}
		body := rest[open+1 : open+closing]
		rest = rest[open+closing+1:]
		fields := strings.Split(body, ";")
		if len(fields) != 7 {
			t.Fatalf("the ace %q in %s does not have seven sddl fields: %q", body, path, sddl)
		}
		entries = append(entries, daclEntry{kind: fields[0], flags: fields[1], rights: fields[2], sid: fields[6]})
	}
}

func describeDACL(entries []daclEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.kind+"/"+e.flags+"/"+e.rights+"/"+e.sid)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func ownerOnlyAccounts(user string) map[string]bool {
	return map[string]bool{user: true, "SY": true, "BA": true}
}

func expectOwnerOnly(t *testing.T, path string, entries []daclEntry) {
	t.Helper()
	user := processUserSID(t)
	want := ownerOnlyAccounts(user)
	if len(entries) != len(want) {
		t.Fatalf("%s carries %d acl entries, want exactly the three accounts that own a dolmen data directory (the user, LocalSystem and the administrators): %s",
			path, len(entries), describeDACL(entries))
	}
	for _, e := range entries {
		if e.kind != "A" {
			t.Fatalf("%s carries a %q entry, and the data directory is owner-only by allow-list: %s", path, e.kind, describeDACL(entries))
		}
		if !want[e.sid] {
			t.Fatalf("%s grants %s access, which is neither the process user nor LocalSystem nor the administrators: %s",
				path, e.sid, describeDACL(entries))
		}
		if e.rights != "FA" && e.rights != "GA" && !strings.EqualFold(e.rights, "0x1f01ff") {
			t.Fatalf("%s grants %s the rights %q, want full control: %s", path, e.sid, e.rights, describeDACL(entries))
		}
	}
}

func TestOpenLeavesTheDataDirectoryOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open a fresh data directory: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	protected, entries := readDACL(t, dir)
	expectOwnerOnly(t, dir, entries)
	if !protected {
		t.Fatalf("the data directory's dacl is not protected, so the parent's entries can still reach it: %s", describeDACL(entries))
	}
	for _, e := range entries {
		if !strings.Contains(e.flags, "OI") || !strings.Contains(e.flags, "CI") {
			t.Fatalf("the entry for %s does not carry the inheritance flags, so a namespace file or a nested namespace directory created afterwards would fall back to the parent's acl: %s", e.sid, describeDACL(entries))
		}
		if strings.Contains(e.flags, "ID") {
			t.Fatalf("the entry for %s is an inherited entry, and the data directory's dacl is supposed to carry nothing from its parent: %s", e.sid, describeDACL(entries))
		}
	}
}

func TestANamespaceFileInheritsTheDataDirectoryACL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open a fresh data directory: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateNamespace(ctx, "acls", [16]byte{}); err != nil {
		t.Fatalf("create a namespace: %v", err)
	}
	_, entries := readDACL(t, filepath.Join(dir, "acls.db"))
	expectOwnerOnly(t, filepath.Join(dir, "acls.db"), entries)
}

func TestOpenTightensADataDirectoryWhoseACLIsLoose(t *testing.T) {
	user := processUserSID(t)
	dir := t.TempDir()
	setDACL(t, dir, "D:P(A;OICI;FA;;;"+user+")(A;OICI;FA;;;BU)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if _, entries := readDACL(t, dir); len(entries) != 4 {
		t.Fatalf("could not widen the fixture's acl, so the test would prove nothing: %s", describeDACL(entries))
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open a data directory with a loose acl: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, entries := readDACL(t, dir)
	expectOwnerOnly(t, dir, entries)
}

func TestBackupOutputDirectoryIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	st, err := Open(data)
	if err != nil {
		t.Fatalf("open a fresh data directory: %v", err)
	}
	if err := st.CreateNamespace(context.Background(), "acls", [16]byte{}); err != nil {
		t.Fatalf("create a namespace: %v", err)
	}
	st.Close()

	out := filepath.Join(dir, "backup")
	if _, err := Backup(context.Background(), data, out, nil); err != nil {
		t.Fatalf("back up into a fresh directory: %v", err)
	}
	_, entries := readDACL(t, out)
	expectOwnerOnly(t, out, entries)
}
