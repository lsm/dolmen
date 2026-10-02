package postgres

import "strings"

const DefaultMaxConns int32 = 20

func poolSize(configured int32, dsn string, parsed int32) int32 {
	if configured > 0 {
		return configured
	}
	if strings.Contains(dsn, "pool_max_conns") {
		return parsed
	}
	if parsed > DefaultMaxConns {
		return DefaultMaxConns
	}
	return parsed
}
