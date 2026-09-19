package postgres

import (
	"encoding/json"
	"math/big"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/value"
)

func decodeNumber(field schema.Field, raw string) (any, error) {
	if number, ok := new(big.Rat).SetString(raw); ok && number.IsInt() && number.Num().IsInt64() {
		return number.Num().Int64(), nil
	}
	return value.Coerce(field, json.Number(raw))
}
