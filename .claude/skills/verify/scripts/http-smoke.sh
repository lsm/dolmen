#!/usr/bin/env bash
# Drive the HTTP /v1 surface of a running serve.sh server with synthetic rows
# and check each response a client would see. Usage: http-smoke.sh <work dir>
set -euo pipefail
W="$1"
here="$(dirname "$0")"
fail=0
check() {
  local name="$1" want="$2" got="$3"
  if printf '%s' "$got" | grep -qF -- "$want"; then
    echo "PASS $name"
  else
    echo "FAIL $name: want substring $want"; echo "     got: $got"; fail=1
  fi
}
call() { "$here/call.sh" "$W" "$@"; }

check create_table '"version":1' "$(call create_table '{"namespace":"verify","table":"events","fields":[{"name":"title","type":"string","fulltext":true},{"name":"score","type":"number"},{"name":"embedding","type":"vector","dim":4}]}')"
check insert '"inserted":2' "$(call insert '{"namespace":"verify","table":"events","records":[{"title":"first bug","score":0.75,"embedding":[0.5,0.25,-0.5,0.0]},{"title":"second note","score":0.1,"embedding":[0,1,0,0]}]}')"
check read_rows '"second note"' "$(call read_rows '{"namespace":"verify","table":"events"}')"
check search_fulltext '"title":"first bug"' "$(call search_fulltext '{"namespace":"verify","table":"events","query":"bug"}')"
check search_vector '"title":"first bug"' "$(call search_vector '{"namespace":"verify","table":"events","vector":[0.5,0.25,-0.5,0.0],"limit":1}')"
check query '"n":2' "$(call query '{"namespace":"verify","sql":"SELECT count(*) AS n FROM events"}')"
check typed_error '"code":"invalid_request"' "$(call insert '{"namespace":"verify","table":"events","records":[{"title":"x","score":"high"}]}')"
claim='{"namespace":"verify","writes":[{"kind":"update","table":"events","filter":"title = ? AND score = ?","args":["second note",0.1],"set":{"score":0.2},"expected_rows":1},{"kind":"insert","table":"events","records":[{"title":"claimed"}]}]}'
check batch_expected_rows '"updated":1' "$(call batch "$claim")"
check batch_expected_rows_stale 'expected_rows mismatch' "$(call batch "$claim")"
check batch_expected_rows_rolled_back '"n":1' "$(call query '{"namespace":"verify","sql":"SELECT count(*) AS n FROM events WHERE title = '"'"'claimed'"'"'"}')"
check batch_expected_rows_null '"code":"invalid_request"' "$(call batch '{"namespace":"verify","writes":[{"kind":"delete","table":"events","filter":"1=1","expected_rows":null}]}')"
imp='{"namespace":"verify","table":"events","idempotency_key":"snap/events/1","records":[{"id":9007199254740993,"created_at":"2026-01-02T03:04:05.123Z","title":"imported row"}]}'
check import_records '"ids":[9007199254740993],"inserted":1,"replayed":false,"skipped":0' "$(call import_records "$imp")"
check import_identity '"created_at":"2026-01-02T03:04:05.123Z","embedding":null,"id":9007199254740993' "$(call read_rows '{"namespace":"verify","table":"events","ids":[9007199254740993]}')"
check import_replay '"replayed":true' "$(call import_records "$imp")"
check import_conflict '"code":"conflict"' "$(call import_records '{"namespace":"verify","table":"events","idempotency_key":"snap/events/2","records":[{"id":9007199254740993,"created_at":"2026-01-02T03:04:05.123Z","title":"changed"}]}')"
check insert_after_import '"ids":[9007199254740994]' "$(call insert '{"namespace":"verify","table":"events","records":[{"title":"after import"}]}')"
check migrate_create '"version":1' "$(call create_table '{"namespace":"verify","table":"constraint_notes","fields":[{"name":"slug","type":"string"}]}')"
check migrate_seed '"inserted":2' "$(call insert '{"namespace":"verify","table":"constraint_notes","records":[{"slug":"dup"},{"slug":"dup"}]}')"
constraint='{"namespace":"verify","table":"constraint_notes","changes":[{"op":"add_constraint","constraint":{"name":"slug_key","kind":"unique","fields":["slug"]}}]}'
check migrate_conflict '"code":"conflict"' "$(call migrate "$constraint")"
check migrate_delete_duplicate '"deleted":1' "$(call delete '{"namespace":"verify","table":"constraint_notes","filter":"id = 2"}')"
check migrate_add_constraint '"version":2' "$(call migrate "$constraint")"
check migrate_constraint_enforced '"code":"conflict"' "$(call insert '{"namespace":"verify","table":"constraint_notes","records":[{"slug":"dup"}]}')"
check migrate_accept_references '"accept_references":true' "$(call migrate '{"namespace":"verify","table":"constraint_notes","changes":[{"op":"set_accept_references","value":true}]}')"
check migrate_standalone 'must run in its own migrate call' "$(call migrate '{"namespace":"verify","table":"constraint_notes","changes":[{"op":"drop_constraint","name":"slug_key"},{"op":"set_accept_references","value":false}]}')"
check migrate_drop_constraint '"version":4' "$(call migrate '{"namespace":"verify","table":"constraint_notes","changes":[{"op":"drop_constraint","name":"slug_key"}]}')"
check migrate_drop_enforced '"inserted":1' "$(call insert '{"namespace":"verify","table":"constraint_notes","records":[{"slug":"dup"}]}')"
check relate_needs_relation 'not a relation table' "$(call relate '{"namespace":"verify","table":"events","source":1,"target":2,"type":"k"}')"
check create_nodes '"accept_references":true' "$(call create_table '{"namespace":"verify","table":"nodes","accept_references":true,"fields":[{"name":"label","type":"string"}]}')"
check create_relation '"relation":{"source":"src","target":"dst","type":"kind"}' "$(call create_table '{"namespace":"verify","table":"links","fields":[{"name":"src","type":"number","required":true},{"name":"dst","type":"number","required":true},{"name":"kind","type":"string","required":true}],"constraints":[{"name":"src_ref","kind":"foreign_key","field":"src","references":{"table":"nodes","field":"id"}},{"name":"dst_ref","kind":"foreign_key","field":"dst","references":{"table":"nodes","field":"id"}}],"relation":{"source":"src","target":"dst","type":"kind"}}')"
check dangling_edge 'src_ref' "$(call insert '{"namespace":"verify","table":"links","records":[{"src":1,"dst":1,"kind":"k"}]}')"
check relate_nodes '"inserted":2' "$(call insert '{"namespace":"verify","table":"nodes","records":[{"label":"n1"},{"label":"n2"}]}')"
check relate '"id":1,"replayed":false' "$(call relate '{"namespace":"verify","table":"links","source":1,"target":2,"type":"k","idempotency_key":"edge-1"}')"
check relate_replay '"id":1,"replayed":true' "$(call relate '{"namespace":"verify","table":"links","source":1,"target":2,"type":"k","idempotency_key":"edge-1"}')"
check relate_dangling 'dst_ref' "$(call relate '{"namespace":"verify","table":"links","source":1,"target":9,"type":"k"}')"
check endpoint_delete_refused '"code":"conflict"' "$(call delete '{"namespace":"verify","table":"nodes","filter":"id = 2"}')"
check openapi '"openapi"' "$(curl -sS "$(cat "$W/url")/v1/openapi.json")"
exit $fail
