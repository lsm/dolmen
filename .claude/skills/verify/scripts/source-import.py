#!/usr/bin/env python3
import json
import os
import pathlib
import sqlite3
import subprocess
import sys
import urllib.error
import urllib.request

work = pathlib.Path(sys.argv[1]).resolve()
container = sys.argv[2]
root = pathlib.Path(__file__).resolve().parents[4]
recipes = root / "examples/import"
url = (work / "url").read_text().strip()
binary = str(work / "dolmen")
env = {k: v for k, v in os.environ.items() if not k.startswith("DOLMEN_") and k not in ("OPENAI_API_KEY", "HF_TOKEN", "REMBED_CACHE")}
env["DOCKER_CONTEXT"] = "colima"

def docker(*args, input=None):
    return subprocess.check_output(["docker", *args], input=input, env=env)

def sql(statement):
    return docker("exec", "-i", container, "psql", "-X", "-qAt", "-U", "dolmen", "-d", "dolmen_test", "-v", "ON_ERROR_STOP=1", input=statement.encode())

def call(op, payload):
    req = urllib.request.Request(url + "/v1/" + op, json.dumps(payload).encode(), {"Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(req)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        result = json.load(response)
    assert result["ok"], result
    return result["data"]

def cli(namespace, table, snapshot, text, *options):
    return subprocess.run([binary, "import", "-url", url, "-namespace", namespace, "-table", table, "-snapshot", snapshot, *options], input=text, text=True, capture_output=True, env=env)

stamp = "2026-01-02T03:04:05."
events = [
    {"id": 42, "created_at": stamp + "123Z", "body": 'granite "archive"\n<text>'},
    {"id": 9007199254740993, "created_at": stamp + "123Z", "body": None},
    {"id": 9007199254740995, "created_at": stamp + "999Z", "body": "never round"},
]
notes = [
    {"id": 7, "created_at": stamp + "000Z", "body": "reference", "event_id": 9007199254740993},
    {"id": 8, "created_at": stamp + "999Z", "body": None, "event_id": 42},
]
expected = {"events": events, "notes": notes}
lite = work / "sqlite-export"
lite.mkdir()
source = lite / "source.db"
with sqlite3.connect(source) as db:
    db.execute("CREATE TABLE events(id INTEGER PRIMARY KEY,created_at TEXT NOT NULL,body TEXT)")
    db.execute("CREATE TABLE notes(id INTEGER PRIMARY KEY,created_at TEXT NOT NULL,body TEXT,event_id INTEGER)")
    for table, rows in expected.items():
        for row in rows:
            db.execute("INSERT INTO " + table + " VALUES(" + ",".join("?" for _ in row) + ")", list(row.values()))
subprocess.run(["sqlite3", "-readonly", "-bail", str(source)], input=(recipes / "sqlite.sql").read_bytes(), cwd=lite, check=True, env=env)
assert json.loads((lite / "precision.json").read_text()) == {"tables": {"events": 0, "notes": 0}, "total": 0}
print("PASS SQLite read-transaction native JSON export")
sql("""
CREATE TABLE events(id bigint PRIMARY KEY,created_at timestamptz NOT NULL,body text);
CREATE TABLE notes(id bigint PRIMARY KEY,created_at timestamptz NOT NULL,body text,event_id bigint);
INSERT INTO events VALUES
(42,'2026-01-02T05:04:05.123456+02','granite "archive"'||chr(10)||'<text>'),
(9007199254740993,'2026-01-02T03:04:05.123000Z',NULL),
(9007199254740995,'2026-01-02T03:04:05.999999Z','never round');
INSERT INTO notes VALUES
(7,'2026-01-02T03:04:05.000001Z','reference',9007199254740993),
(8,'2026-01-02T03:04:05.999000Z',NULL,42);
""")
docker("exec", container, "mkdir", "/tmp/source-import-export")
docker("exec", "-i", "-w", "/tmp/source-import-export", container, "psql", "-X", "-qAt", "-U", "dolmen", "-d", "dolmen_test", input=(recipes / "postgres.sql").read_bytes())
pg = work / "postgres-export"
pg.mkdir()
for name in ("events.jsonl", "notes.jsonl", "precision.json"):
    (pg / name).write_bytes(docker("exec", container, "cat", "/tmp/source-import-export/" + name))
assert json.loads((pg / "precision.json").read_text()) == {"tables": {"events": 2, "notes": 1}, "total": 3}
print("PASS PostgreSQL repeatable-read read-only export; precision loss events=2 notes=1 total=3")
for label, directory in (("sqlite", lite), ("postgres", pg)):
    namespace = "source_" + label
    for table, rows in expected.items():
        lines = (directory / (table + ".jsonl")).read_text().splitlines()
        assert [json.loads(line) for line in lines] == rows
        fields = [{"name": "body", "type": "text", "fulltext": True}]
        if table == "notes":
            fields.append({"name": "event_id", "type": "number"})
        call("create_table", {"namespace": namespace, "table": table, "fields": fields})
        text = "\n".join(lines) + "\n"
        interrupted = cli(namespace, table, "snapshot", lines[0] + '\n{"NEVER-ECHO":\n', "-rows", "1")
        assert interrupted.returncode != 0 and "committed chunk 1" in interrupted.stdout
        assert "line 2" in interrupted.stderr and "NEVER-ECHO" not in interrupted.stderr
        resumed = cli(namespace, table, "snapshot", text, "-rows", "1")
        assert resumed.returncode == 0 and resumed.stdout.count("committed chunk") == len(rows), resumed.stderr
        assert resumed.stdout.count("replayed=true") == 1
        replay = cli(namespace, table, "snapshot", text, "-rows", "1")
        assert replay.returncode == 0 and replay.stdout.count("replayed=true") == len(rows)
        regrouped = cli(namespace, table, "new-snapshot", text, "-rows", "2", "-bytes", "350")
        assert regrouped.returncode == 0 and "inserted=0" in regrouped.stdout and "skipped=" in regrouped.stdout
        actual = call("query", {"namespace": namespace, "sql": "SELECT * FROM " + table + " ORDER BY id"})["rows"]
        assert actual == rows, (label, table, actual)
        changes = call("changes_since", {"namespace": namespace, "table": table, "cursor": "begin"})["changes"]
        assert len(changes) == len(rows) and all(c["kind"] == "insert" for c in changes), changes
        changed = [dict(rows[0], id=99), dict(rows[0], body="NEVER-ECHO")]
        conflict = cli(namespace, table, "conflicting", "\n".join(json.dumps(r) for r in changed) + "\n")
        assert conflict.returncode != 0 and "line 2" in conflict.stderr and "conflict" in conflict.stderr
        assert "NEVER-ECHO" not in conflict.stderr and not conflict.stdout
        assert call("query", {"namespace": namespace, "sql": "SELECT * FROM " + table + " ORDER BY id"})["rows"] == rows
        print("PASS " + label + " " + table + ": interrupted resume, replay, regrouped skip, exact rows, no duplicate changes, atomic conflict")
    assert call("search_fulltext", {"namespace": namespace, "table": "events", "query": "granite"})["results"]
    print("PASS " + label + ": rebuilt fulltext index; references and truncated timestamps compare exactly")
(work / "source-import.json").write_text(json.dumps({"verified": True, "sourceRows": {"events": 3, "notes": 2}, "postgresPrecisionLoss": {"events": 2, "notes": 1, "total": 3}, "sources": ["sqlite", "postgres"]}, indent=2) + "\n")
