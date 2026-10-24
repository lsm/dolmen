\set ON_ERROR_STOP on
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL TIME ZONE 'UTC';
\o events.jsonl
SELECT json_build_object('id',id,'created_at',to_char(date_trunc('milliseconds',created_at),'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),'body',body) FROM events ORDER BY id;
\o notes.jsonl
SELECT json_build_object('id',id,'created_at',to_char(date_trunc('milliseconds',created_at),'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),'body',body,'event_id',event_id) FROM notes ORDER BY id;
\o precision.json
WITH losses AS (
 SELECT 'events' AS name,count(*) FILTER (WHERE created_at<>date_trunc('milliseconds',created_at)) AS lost FROM events
 UNION ALL SELECT 'notes',count(*) FILTER (WHERE created_at<>date_trunc('milliseconds',created_at)) FROM notes
) SELECT json_build_object('tables',json_object_agg(name,lost),'total',sum(lost)) FROM losses;
\o
COMMIT;
