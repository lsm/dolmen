package lakehouse

import (
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestTheChangeFeedQueriesUseAnIndex(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ChangesSince(ctx, "ns", "", "", [16]byte{}, nil, store.Incarnation{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	db := s.namespaces["ns"].db
	for _, q := range []string{
		`SELECT seq FROM _dolmen_lakehouse_changes WHERE table_name = 'a' AND generation = 0 AND seq > 0`,
		`SELECT seq FROM _dolmen_lakehouse_changes WHERE table_name = 'a' AND generation = 0 AND owner = 'x' AND seq > 0`,
		`DELETE FROM _dolmen_lakehouse_changes WHERE at < '2026'`,
		`DELETE FROM _dolmen_lakehouse_cursors WHERE issued_at < 0`,
		`SELECT min(chain_origin) FROM _dolmen_lakehouse_cursors`,
	} {
		rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "INDEX") {
			t.Errorf("%s scans without an index: %s", q, joined)
		}
	}
}
