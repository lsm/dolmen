package postgres

import "testing"

func TestPoolSizeDefaultsBelowTheDatabaseLimit(t *testing.T) {
	cases := []struct {
		name       string
		configured int32
		dsn        string
		parsed     int32
		want       int32
	}{
		{"large host default is capped", 0, "postgres://h/db", 128, DefaultMaxConns},
		{"small host default is kept", 0, "postgres://h/db", 4, 4},
		{"dsn pool_max_conns wins", 0, "postgres://h/db?pool_max_conns=60", 60, 60},
		{"keyword dsn pool_max_conns wins", 0, "host=h dbname=db pool_max_conns=60", 60, 60},
		{"explicit config wins", 35, "postgres://h/db?pool_max_conns=60", 60, 35},
	}
	for _, c := range cases {
		if got := poolSize(c.configured, c.dsn, c.parsed); got != c.want {
			t.Fatalf("%s: pool size %d, want %d", c.name, got, c.want)
		}
	}
}
