.bail on
.headers off
.mode list
PRAGMA query_only=ON;
BEGIN;
.output events.jsonl
SELECT json_object('id',id,'created_at',created_at,'body',body) FROM events ORDER BY id;
.output notes.jsonl
SELECT json_object('id',id,'created_at',created_at,'body',body,'event_id',event_id) FROM notes ORDER BY id;
.output precision.json
SELECT json_object('tables',json_object('events',0,'notes',0),'total',0);
.output stdout
COMMIT;
