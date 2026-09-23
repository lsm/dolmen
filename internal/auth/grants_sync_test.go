package auth

import "testing"

func TestTheGrantRegistrySurvivesPowerLoss(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got int
	if err := r.db.QueryRow(`PRAGMA synchronous`).Scan(&got); err != nil || got != 2 {
		t.Fatalf("grant registry runs synchronous=%d (%v); a revocation must survive power loss, so it needs FULL (2)", got, err)
	}
}
