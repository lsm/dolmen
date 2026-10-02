package driver

import (
	"testing"

	"github.com/lsm/dolmen/spike/driver/fixture"
)

func TestTheSidecarReadsTheFixtureAtTheCurrentSnapshotWithTheDeletesApplied(t *testing.T) {
	const seeded = 50
	tbl := newFixture(t, seeded)
	_, sc, _ := locked(t, tbl)

	res := mustQuery(t, sc, "SELECT count(*) FROM events")
	want := seeded + 1 - len(fixture.DeletedPositions())
	if got := countOf(t, res); got != want {
		t.Fatalf("count(*) is %d, want %d (%d seeded, one late arrival, %d position-deleted)",
			got, want, seeded, len(fixture.DeletedPositions()))
	}
}

func TestTheSidecarAppliesThePositionDeleteFilesRatherThanCountingEveryRow(t *testing.T) {
	const seeded = 50
	tbl := newFixture(t, seeded)
	_, sc, _ := locked(t, tbl)

	res := mustQuery(t, sc, "SELECT count(*) FROM events WHERE live")
	if res.Rows[0] == "" {
		t.Fatal("no count row came back for the predicate read")
	}
	deleted := 0
	for _, p := range fixture.DeletedPositions() {
		if p%3 == 0 {
			deleted++
		}
	}
	want := seeded + 1 - deleted
	if got := countOf(t, res); got != want {
		t.Fatalf("count of live rows is %d, want %d; the delete files were not applied", got, want)
	}
}

func TestTheDeletedRowsAreGoneFromTheResultSetNotJustTheCount(t *testing.T) {
	const seeded = 50
	tbl := newFixture(t, seeded)
	_, sc, _ := locked(t, tbl)

	res := mustQuery(t, sc, "SELECT id FROM events WHERE id <= 6")
	for _, row := range rowsOf(res) {
		if row == "2" || row == "5" {
			t.Fatalf("row %s was returned although it is position-deleted; the result was %q", row, res.Rows)
		}
	}
}

func TestTheSidecarReadsTheOlderSnapshotBeforeTheDeletesWereCommitted(t *testing.T) {
	const seeded = 50
	tbl := newFixture(t, seeded)
	e := requireEngine(t)
	ids := fixture.SnapshotIDs(tbl)
	if len(ids) < 2 {
		t.Fatalf("want at least two snapshots to travel between, got %v", ids)
	}
	older := ids[0]

	sc := e.start(t, rootOf(tbl), older, "events", true)
	res := mustQuery(t, sc, "SELECT count(*) FROM events")
	if got := countOf(t, res); got != seeded {
		t.Fatalf("at snapshot %d the count is %d, want %d: the snapshot was not pinned",
			older, got, seeded)
	}
}

func TestTheSidecarRefusesASnapshotThatIsNotInTheTable(t *testing.T) {
	tbl := newFixture(t, 10)
	e := requireEngine(t)

	_, err := tryInit(t, e, rootOf(tbl), 1, "events")
	if err == nil {
		t.Fatal("init accepted a snapshot id the table does not contain")
	}
}

func TestTheSidecarReportsTheColumnTypesItWillDisclose(t *testing.T) {
	tbl := newFixture(t, 10)
	_, sc, _ := locked(t, tbl)

	res := mustQuery(t, sc, "SELECT id, body, score FROM events LIMIT 1")
	if len(res.Columns) != 3 {
		t.Fatalf("want three columns, got %+v", res.Columns)
	}
	seen := map[string]bool{}
	for _, c := range res.Columns {
		seen[c.Name] = true
		if c.Type == "" {
			t.Fatalf("column %s came back with no type, so query_dialect could not disclose it", c.Name)
		}
	}
	for _, want := range []string{"id", "body", "score"} {
		if !seen[want] {
			t.Fatalf("column %s is missing from %+v", want, res.Columns)
		}
	}
}

func TestTheSidecarSurvivesAQueryThatDoesNotExist(t *testing.T) {
	tbl := newFixture(t, 10)
	_, sc, _ := locked(t, tbl)

	if _, err := query(t, sc, "SELECT * FROM no_such_table"); err == nil {
		t.Fatal("a query against a table that does not exist was accepted")
	}
	if _, err := query(t, sc, "SELECT nonsense_function(1)"); err == nil {
		t.Fatal("a query using a function that does not exist was accepted")
	}
	res := mustQuery(t, sc, "SELECT count(*) FROM events")
	if res.Rows == nil {
		t.Fatal("the sidecar did not return to serving queries after two errors")
	}
}
