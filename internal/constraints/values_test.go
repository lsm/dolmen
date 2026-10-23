package constraints

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func keyFor(t *testing.T, kind schema.FieldType, input any, owner *string) string {
	t.Helper()
	key, participates, err := UniqueKey([]schema.Field{{Name: "value", Type: kind}}, map[string]any{"value": input}, owner)
	if err != nil || !participates {
		t.Fatalf("key: participates=%v, err=%v", participates, err)
	}
	return key
}

func TestUniqueNumbersPreserveExactIntegersAndFloatEquivalence(t *testing.T) {
	for _, v := range []any{int64(1), float64(1), float32(1), json.Number("1.0"), uint32(1)} {
		if keyFor(t, schema.Number, v, nil) != keyFor(t, schema.Number, int64(1), nil) {
			t.Fatalf("one differs for %T", v)
		}
	}
	if keyFor(t, schema.Number, int64(0), nil) != keyFor(t, schema.Number, math.Copysign(0, -1), nil) {
		t.Fatal("signed zero must equal integer zero")
	}
	if keyFor(t, schema.Number, int64(9007199254740993), nil) == keyFor(t, schema.Number, float64(9007199254740992), nil) {
		t.Fatal("integer rounded through float64")
	}
	if keyFor(t, schema.Number, int64(math.MaxInt64), nil) == keyFor(t, schema.Number, float64(math.MaxInt64), nil) {
		t.Fatal("max int64 collided with 2^63")
	}
	if keyFor(t, schema.Number, json.Number("9223372036854775807"), nil) != keyFor(t, schema.Number, int64(math.MaxInt64), nil) {
		t.Fatal("JSON integer lost fidelity")
	}
	if keyFor(t, schema.Number, json.Number("0.1"), nil) != keyFor(t, schema.Number, float64(0.1), nil) {
		t.Fatal("decimal input must use normal value coercion")
	}
}

func TestUniqueKeysPreserveTextAndTupleBoundaries(t *testing.T) {
	fields := []schema.Field{{Name: "first", Type: schema.String}, {Name: "second", Type: schema.String}}
	a, _, err := UniqueKey(fields, map[string]any{"first": "ab", "second": "c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := UniqueKey(fields, map[string]any{"first": "a", "second": "bc"}, nil)
	if err != nil || a == b {
		t.Fatalf("tuple boundaries collapsed: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range []string{"A", "a", "é", "e\u0301", "\xff", "\xfe", "�", "a:b", "a|b"} {
		key := keyFor(t, schema.String, s, nil)
		if seen[key] {
			t.Fatal("distinct byte strings collided")
		}
		seen[key] = true
	}
	if keyFor(t, schema.Boolean, true, nil) != keyFor(t, schema.Boolean, int64(1), nil) {
		t.Fatal("stored boolean must match typed boolean")
	}
}

func TestUniqueTimestampKeysUseCanonicalStringsNotInstants(t *testing.T) {
	utc := keyFor(t, schema.Timestamp, "2026-10-23T12:00:00Z", nil)
	if utc != keyFor(t, schema.Timestamp, " 2026-10-23t12:00:00z ", nil) {
		t.Fatal("timestamp coercion not applied")
	}
	if utc == keyFor(t, schema.Timestamp, "2026-10-23T08:00:00-04:00", nil) {
		t.Fatal("equivalent instants with different offsets must remain distinct")
	}
	if keyFor(t, schema.Timestamp, "2026-10-23T12:00:00.0Z", nil) == keyFor(t, schema.Timestamp, "2026-10-23T12:00:00.00Z", nil) {
		t.Fatal("stored fractional precision must be preserved")
	}
	if keyFor(t, schema.Timestamp, "2026-10-23", nil) == keyFor(t, schema.Timestamp, "2026-10-23T00:00:00Z", nil) {
		t.Fatal("date-only form must remain distinct")
	}
}

func TestUniqueOwnerDomainsDistinguishUnownedEmptyAndPrincipals(t *testing.T) {
	empty, alice, bob := "", "alice", "bob"
	seen := map[string]bool{}
	for _, owner := range []*string{nil, &empty, &alice, &bob} {
		key := keyFor(t, schema.String, "same", owner)
		if seen[key] {
			t.Fatal("owner domains collided")
		}
		seen[key] = true
		if !SameOwner(owner, owner) {
			t.Fatal("owner domain must match itself")
		}
	}
	aliceCopy := "alice"
	if !SameOwner(&alice, &aliceCopy) || SameOwner(nil, &empty) || SameOwner(&alice, &bob) {
		t.Fatal("owner equality must use the labelled domain, not pointer identity")
	}
}

func TestUniqueNullDoesNotHideOtherMalformedKeyValues(t *testing.T) {
	fields := []schema.Field{{Name: "first", Type: schema.String}, {Name: "second", Type: schema.Number}}
	for _, row := range []map[string]any{nil, {"first": nil, "second": int64(1)}, {"first": "x", "second": nil}} {
		key, participates, err := UniqueKey(fields, row, nil)
		if err != nil || participates || key != "" {
			t.Fatalf("NULL participation: %v, %v", participates, err)
		}
	}
	if _, _, err := UniqueKey(fields, map[string]any{"first": nil, "second": math.NaN()}, nil); err == nil {
		t.Fatal("NULL key must not hide malformed numeric values")
	}
}

func TestInvalidConstraintValuesFailWithoutEchoingValues(t *testing.T) {
	for _, v := range []any{math.NaN(), math.Inf(1), math.Inf(-1), uint64(math.MaxUint64), "private-value"} {
		_, _, err := UniqueKey([]schema.Field{{Name: "quantity", Type: schema.Number}}, map[string]any{"quantity": v}, nil)
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("invalid number error: %v", err)
		}
	}
	for _, kind := range []schema.FieldType{schema.JSON, schema.Vector, schema.Secret, schema.File} {
		if _, _, err := UniqueKey([]schema.Field{{Name: "value", Type: kind}}, map[string]any{"value": "private-value"}, nil); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsupported %s error: %v", kind, err)
		}
	}
	for _, count := range []int{0, 9} {
		if _, _, err := UniqueKey(make([]schema.Field, count), nil, nil); err == nil {
			t.Fatal("invalid unique key size accepted")
		}
	}
}

func TestReferenceIDsPreserveInt64AndRejectRounding(t *testing.T) {
	for _, v := range []any{int64(math.MaxInt64), json.Number("9223372036854775807")} {
		id, present, err := ReferenceID(v)
		if err != nil || !present || id != math.MaxInt64 {
			t.Fatalf("max ID: %d, %v, %v", id, present, err)
		}
	}
	for _, v := range []any{int(1), float64(1), json.Number("1.0")} {
		id, present, err := ReferenceID(v)
		if err != nil || !present || id != 1 {
			t.Fatalf("ID one: %d, %v, %v", id, present, err)
		}
	}
	if id, present, err := ReferenceID(nil); err != nil || present || id != 0 {
		t.Fatalf("NULL ID: %d, %v, %v", id, present, err)
	}
	for _, v := range []any{int64(0), int64(-1), float64(0.5), float64(math.MaxInt64), math.Inf(1), math.NaN(), uint64(math.MaxUint64), "private-reference", json.Number("9223372036854775807.0")} {
		_, _, err := ReferenceID(v)
		if err == nil || strings.Contains(err.Error(), "private-reference") {
			t.Fatalf("invalid reference %T: %v", v, err)
		}
	}
}
