package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func principal(id string) Subject { return Subject{Type: SubjectPrincipal, ID: id} }
func group(id string) Subject     { return Subject{Type: SubjectGroup, ID: id} }

func mustGrant(t *testing.T, r *Registry, subj Subject, obj Object, verbs ...Verb) Grant {
	t.Helper()
	g, err := r.Grant(context.Background(), subj, obj, NewVerbSet(verbs...))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return g
}

func verbsOn(t *testing.T, r *Registry, id Identity, obj Object) VerbSet {
	t.Helper()
	v, err := r.EffectiveVerbs(context.Background(), id, obj)
	if err != nil {
		t.Fatalf("effective verbs: %v", err)
	}
	return v
}

func TestGrantsInheritDownward(t *testing.T) {
	r := newRegistry(t)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbRead)
	alice := Identity{Principal: "alice"}

	for _, obj := range []Object{
		{Namespace: "acme"},
		{Namespace: "acme", Table: "docs"},
		{Namespace: "acme/team-a"},
		{Namespace: "acme/team-a", Table: "events"},
		{Namespace: "acme/team-a/sub", Table: "t"},
	} {
		if !verbsOn(t, r, alice, obj).Has(VerbRead) {
			t.Fatalf("read did not inherit to %s", obj)
		}
	}
	if !verbsOn(t, r, alice, Object{Namespace: "other"}).Empty() {
		t.Fatal("a grant on acme reached a sibling namespace")
	}
	if !verbsOn(t, r, alice, Object{Namespace: RootObject}).Empty() {
		t.Fatal("a namespace grant reached the root object")
	}
}

func TestRootGrantCoversEverything(t *testing.T) {
	r := newRegistry(t)
	mustGrant(t, r, principal("root"), Object{Namespace: RootObject}, VerbAdmin)
	id := Identity{Principal: "root"}
	for _, obj := range []Object{{Namespace: RootObject}, {Namespace: "anything"}, {Namespace: "a/b", Table: "t"}} {
		if !verbsOn(t, r, id, obj).Has(VerbAdmin) {
			t.Fatalf("root admin did not cover %s", obj)
		}
	}
}

func TestTableGrantDoesNotInheritUpward(t *testing.T) {
	r := newRegistry(t)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme", Table: "docs"}, VerbRead)
	alice := Identity{Principal: "alice"}
	if !verbsOn(t, r, alice, Object{Namespace: "acme", Table: "docs"}).Has(VerbRead) {
		t.Fatal("table grant does not cover its own table")
	}
	if !verbsOn(t, r, alice, Object{Namespace: "acme"}).Empty() {
		t.Fatal("a table grant leaked up to the namespace")
	}
	if !verbsOn(t, r, alice, Object{Namespace: "acme", Table: "other"}).Empty() {
		t.Fatal("a table grant leaked to a sibling table")
	}
}

func TestGroupGrantsUnionWithPrincipalGrants(t *testing.T) {
	r := newRegistry(t)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbRead)
	mustGrant(t, r, group("writers"), Object{Namespace: "acme"}, VerbCreate, VerbUpdate)

	alice := Identity{Principal: "alice", Groups: []string{"writers", "unrelated"}}
	got := verbsOn(t, r, alice, Object{Namespace: "acme"})
	if !got.HasAll(VerbRead, VerbCreate, VerbUpdate) {
		t.Fatalf("union over principal and group grants gave %v", got.Strings())
	}
	if got.Has(VerbAdmin) || got.Has(VerbDelete) {
		t.Fatalf("union invented verbs: %v", got.Strings())
	}

	bob := Identity{Principal: "bob", Groups: []string{"writers"}}
	if v := verbsOn(t, r, bob, Object{Namespace: "acme"}); v.Has(VerbRead) {
		t.Fatal("bob inherited alice's principal grant through the shared group")
	}
}

func TestPrincipalAndGroupSubjectsNeverCollide(t *testing.T) {
	r := newRegistry(t)
	mustGrant(t, r, group("alice"), Object{Namespace: "acme"}, VerbAdmin)
	if v := verbsOn(t, r, Identity{Principal: "alice"}, Object{Namespace: "acme"}); !v.Empty() {
		t.Fatalf("principal alice matched the group subject alice: %v", v.Strings())
	}
}

func TestGrantIsIdempotentAndMergesVerbs(t *testing.T) {
	r := newRegistry(t)
	first := mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbRead)
	again := mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbRead)
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("re-granting the same verbs changed created_at")
	}
	merged := mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbCreate)
	if !merged.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("merging a verb changed created_at")
	}
	if !merged.Verbs.HasAll(VerbRead, VerbCreate) {
		t.Fatalf("merge lost a verb: %v", merged.Verbs.Strings())
	}
}

func TestRevokeSemantics(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	obj := Object{Namespace: "acme"}
	mustGrant(t, r, principal("alice"), obj, VerbRead, VerbCreate)

	g, err := r.Revoke(ctx, principal("alice"), obj, NewVerbSet(VerbDelete), false, Reach{})
	if err != nil {
		t.Fatalf("revoking an unheld verb failed: %v", err)
	}
	if g == nil || !g.Verbs.HasAll(VerbRead, VerbCreate) {
		t.Fatalf("revoking an unheld verb changed the grant: %+v", g)
	}

	g, err = r.Revoke(ctx, principal("alice"), obj, NewVerbSet(VerbCreate), false, Reach{})
	if err != nil || g == nil || g.Verbs.Has(VerbCreate) || !g.Verbs.Has(VerbRead) {
		t.Fatalf("partial revoke: %+v %v", g, err)
	}

	g, err = r.Revoke(ctx, principal("alice"), obj, NewVerbSet(VerbRead), false, Reach{})
	if err != nil {
		t.Fatalf("final revoke: %v", err)
	}
	if g != nil {
		t.Fatalf("revoking the last verb left a grant: %+v", g)
	}
	if !verbsOn(t, r, Identity{Principal: "alice"}, obj).Empty() {
		t.Fatal("the grant survived its last verb being revoked")
	}

	if g, err := r.Revoke(ctx, principal("nobody"), obj, NewVerbSet(VerbRead), false, Reach{}); err != nil || g != nil {
		t.Fatalf("revoking a nonexistent grant: %+v %v", g, err)
	}
}

func TestListGrantsOrderAndFilters(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, principal("bob"), Object{Namespace: "acme"}, VerbRead)
	mustGrant(t, r, group("team"), Object{Namespace: "acme", Table: "docs"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme", Table: "docs"}, VerbCreate)
	mustGrant(t, r, principal("alice"), Object{Namespace: "zeta"}, VerbRead)

	all, err := r.List(ctx, nil, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var order []string
	for _, g := range all {
		order = append(order, g.Subject.Type+":"+g.Subject.ID+":"+g.Object.String())
	}
	want := "group:team:acme.docs,principal:alice:acme,principal:alice:acme.docs,principal:alice:zeta,principal:bob:acme"
	if strings.Join(order, ",") != want {
		t.Fatalf("list order:\n got %s\nwant %s", strings.Join(order, ","), want)
	}

	byAlice, err := r.List(ctx, &Subject{Type: SubjectPrincipal, ID: "alice"}, nil)
	if err != nil || len(byAlice) != 3 {
		t.Fatalf("subject filter returned %d grants: %v", len(byAlice), err)
	}

	sub, err := r.List(ctx, nil, &Object{Namespace: "acme"})
	if err != nil {
		t.Fatalf("object filter: %v", err)
	}
	for _, g := range sub {
		if g.Object.Namespace != "acme" {
			t.Fatalf("object subtree filter returned %s", g.Object)
		}
	}
	if len(sub) != 4 {
		t.Fatalf("object subtree filter returned %d grants, want 4", len(sub))
	}
}

func TestNamespaceGrantsSortBeforeTablesInside(t *testing.T) {
	r := newRegistry(t)
	mustGrant(t, r, principal("a"), Object{Namespace: "acme", Table: "team"}, VerbRead)
	mustGrant(t, r, principal("a"), Object{Namespace: "acme/team"}, VerbRead)
	all, err := r.List(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if all[0].Object.String() != "acme.team" || all[1].Object.String() != "acme/team" {
		t.Fatalf("namespace and table objects tie-broke wrongly: %s then %s", all[0].Object, all[1].Object)
	}
}

func TestDropCascades(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme", Table: "docs"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "acme/team"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "keep"}, VerbRead)

	if err := r.DropTable(ctx, "acme", "docs"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if !verbsOn(t, r, Identity{Principal: "alice"}, Object{Namespace: "acme", Table: "docs"}).Empty() {
		t.Log("table grant removed but the namespace grant still inherits, which is correct")
	}
	all, _ := r.List(ctx, nil, &Object{Namespace: "acme", Table: "docs"})
	if len(all) != 0 {
		t.Fatalf("dropping a table left %d grants targeting it", len(all))
	}

	if err := r.DropNamespace(ctx, "acme"); err != nil {
		t.Fatalf("drop namespace: %v", err)
	}
	remaining, _ := r.List(ctx, nil, nil)
	if len(remaining) != 1 || remaining[0].Object.Namespace != "keep" {
		t.Fatalf("dropping a namespace did not remove its subtree grants: %+v", remaining)
	}
}

func TestRootAdminsCountsOnlyAdminVerb(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, principal("reader"), Object{Namespace: RootObject}, VerbRead)
	admins, err := r.RootAdmins(ctx)
	if err != nil || len(admins) != 0 {
		t.Fatalf("a root read grant counted as an administrator: %+v %v", admins, err)
	}
	mustGrant(t, r, principal("boss"), Object{Namespace: RootObject}, VerbAdmin)
	admins, _ = r.RootAdmins(ctx)
	if len(admins) != 1 || admins[0].ID != "boss" {
		t.Fatalf("root administrators: %+v", admins)
	}
}

func TestSubjectValidation(t *testing.T) {
	for name, s := range map[string]Subject{
		"unknown type":        {Type: "user", ID: "alice"},
		"empty principal":     {Type: SubjectPrincipal, ID: ""},
		"space in principal":  {Type: SubjectPrincipal, ID: "alice smith"},
		"reserved principal":  {Type: SubjectPrincipal, ID: AdminPrincipal},
		"comma in group":      {Type: SubjectGroup, ID: "a,b"},
		"non ascii principal": {Type: SubjectPrincipal, ID: "alicé"},
	} {
		if err := s.Valid(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, s := range []Subject{
		{Type: SubjectPrincipal, ID: "alice"},
		{Type: SubjectGroup, ID: "team-a"},
		{Type: SubjectGroup, ID: AdminPrincipal},
	} {
		if err := s.Valid(); err != nil {
			t.Fatalf("%+v rejected: %v", s, err)
		}
	}
}

func TestVerbSerializationOrderIsFixed(t *testing.T) {
	set := NewVerbSet(VerbAdmin, VerbRead, VerbCreate)
	got := strings.Join(set.Strings(), ",")
	if got != "create,read,admin" {
		t.Fatalf("verbs serialized as %q, want the fixed section 2 order", got)
	}
	if _, err := ParseVerbs([]string{"read", "read"}); err == nil {
		t.Fatal("duplicate verbs accepted")
	}
	if _, err := ParseVerbs([]string{"write"}); err == nil {
		t.Fatal("unknown verb accepted")
	}
	if _, err := ParseVerbs(nil); err == nil {
		t.Fatal("empty verb list accepted")
	}
}

func TestLikeWildcardsInNamespacesDoNotMatchSiblings(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, principal("alice"), Object{Namespace: "team_a"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "team_a/sub"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "team-a"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "team-a/sub"}, VerbRead)

	sub, err := r.List(ctx, nil, &Object{Namespace: "team_a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, g := range sub {
		if strings.HasPrefix(g.Object.Namespace, "team-a") {
			t.Fatalf("the subtree filter for team_a matched the distinct namespace %s", g.Object.Namespace)
		}
	}
	if len(sub) != 2 {
		t.Fatalf("subtree filter returned %d grants, want 2: %+v", len(sub), sub)
	}

	if err := r.DropNamespace(ctx, "team_a"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	remaining, _ := r.List(ctx, nil, nil)
	if len(remaining) != 2 {
		t.Fatalf("dropping team_a removed %d grants, want only its own subtree: %+v", 4-len(remaining), remaining)
	}
	for _, g := range remaining {
		if !strings.HasPrefix(g.Object.Namespace, "team-a") {
			t.Fatalf("dropping team_a deleted the grants of %s", g.Object.Namespace)
		}
	}
}

func TestPercentInNamespaceIsNotAWildcard(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, principal("alice"), Object{Namespace: "a"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "a/b"}, VerbRead)
	mustGrant(t, r, principal("alice"), Object{Namespace: "keep"}, VerbRead)

	if err := r.DropNamespace(ctx, "%"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	remaining, _ := r.List(ctx, nil, nil)
	if len(remaining) != 3 {
		t.Fatalf("a namespace of %q deleted %d grants: %+v", "%", 3-len(remaining), remaining)
	}
}

func TestRevokeKeepsTheLastRootAdministrator(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	root := Object{Namespace: RootObject}
	mustGrant(t, r, principal("alice"), root, VerbAdmin)
	mustGrant(t, r, principal("bob"), root, VerbAdmin)

	if _, err := r.Revoke(ctx, principal("bob"), root, NewVerbSet(VerbAdmin), true, Reach{Header: true}); err != nil {
		t.Fatalf("revoking one of two root administrators: %v", err)
	}
	_, err := r.Revoke(ctx, principal("alice"), root, NewVerbSet(VerbAdmin), true, Reach{Header: true})
	if !errors.Is(err, ErrLastRootAdmin) {
		t.Fatalf("revoking the last root administrator returned %v, want ErrLastRootAdmin", err)
	}
	if admins, _ := r.RootAdmins(ctx); len(admins) != 1 {
		t.Fatalf("the refused revoke still changed the grant: %+v", admins)
	}

	if _, err := r.Revoke(ctx, principal("alice"), root, NewVerbSet(VerbAdmin), false, Reach{}); err != nil {
		t.Fatalf("the bootstrap key should let the last root grant go: %v", err)
	}
	if admins, _ := r.RootAdmins(ctx); len(admins) != 0 {
		t.Fatalf("root administrators remain: %+v", admins)
	}
}

func TestConcurrentLastAdminRevokesCannotBothWin(t *testing.T) {
	r := newRegistry(t)
	ctx := context.Background()
	root := Object{Namespace: RootObject}
	mustGrant(t, r, principal("alice"), root, VerbAdmin)
	mustGrant(t, r, principal("bob"), root, VerbAdmin)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, who := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(i int, who string) {
			defer wg.Done()
			_, errs[i] = r.Revoke(ctx, principal(who), root, NewVerbSet(VerbAdmin), true, Reach{Header: true})
		}(i, who)
	}
	wg.Wait()

	admins, err := r.RootAdmins(ctx)
	if err != nil {
		t.Fatalf("root admins: %v", err)
	}
	if len(admins) == 0 {
		t.Fatalf("both revokes committed and left no root administrator: %v", errs)
	}
}
