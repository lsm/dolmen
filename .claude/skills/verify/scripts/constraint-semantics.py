import concurrent.futures
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path

base = Path(sys.argv[1], 'url').read_text().strip()
ns = 'verify_semantics'


def request(op, body):
    req = urllib.request.Request(base + '/v1/' + op, json.dumps(body).encode(), {'Content-Type': 'application/json'})
    try:
        response = urllib.request.urlopen(req, timeout=30)
    except urllib.error.HTTPError as e:
        response = e
    with response:
        return response.status, json.load(response)


def call(op, body, error=None):
    status, out = request(op, body)
    if error:
        assert status == 400 and out['error']['code'] == error, (op, status, out)
        return out['error']
    assert status == 200 and out['ok'], (op, status, out)
    return out['data']


def events(changes):
    return [{key: value for key, value in change.items() if key != 'cursor'} for change in changes]


call('create_namespace', {'namespace': ns})
keys = {'namespace': ns, 'table': 'keys'}
call('create_table', {**keys, 'fields': [{'name': 'n', 'type': 'number'}, {'name': 'observed_at', 'type': 'timestamp'}, {'name': 'tag', 'type': 'string'}], 'constraints': [{'name': 'row_key', 'kind': 'unique', 'fields': ['n', 'observed_at', 'tag']}]})
records = [{'n': n, 'observed_at': stamp, 'tag': tag} for n, stamp, tag in [
    (9223372036854775807, '2026-10-23T12:00:00Z', 'a'),
    (9223372036854775806, '2026-10-23T12:00:00Z', 'a'),
    (1, '2026-10-23T08:00:00-04:00', 'a'),
    (1, '2026-10-23T12:00:00.000Z', 'a'),
    (1, '2026-10-23T12:00:00Z', 'a'),
    (1, '2026-10-23T12:00:00Z', 'A'),
    (1, '2026-10-23T12:00:00Z', None),
    (1, '2026-10-23T12:00:00Z', None),
]]
assert call('insert', {**keys, 'records': records})['inserted'] == 8
for record in ({'n': 1.0, 'observed_at': '2026-10-23T08:00:00-04:00', 'tag': 'a'}, records[0]):
    assert 'row_key' in call('insert', {**keys, 'records': [record]}, 'conflict')['message']
print('PASS exact numeric keys, stored timestamp strings, case and NULL components')
items = {'namespace': ns, 'table': 'items'}
call('create_table', {**items, 'fields': [{'name': 'slug', 'type': 'string'}, {'name': 'retired_at', 'type': 'timestamp'}], 'constraints': [{'name': 'active_slug', 'kind': 'unique', 'fields': ['slug'], 'where': 'retired_at IS NULL'}]})
call('insert', {**items, 'records': [{'slug': 'a'}, {'slug': 'a', 'retired_at': '2026-01-01T00:00:00Z'}]})
call('update', {**items, 'filter': 'id = 2', 'set': {'retired_at': None}}, 'conflict')
call('update', {**items, 'filter': 'id = 1', 'set': {'retired_at': '2026-02-01T00:00:00Z'}})
call('update', {**items, 'filter': 'id = 2', 'set': {'retired_at': None}})
print('PASS partial-key predicate transitions')
rows = call('read_rows', items)['rows']
changes = call('changes_since', {**items, 'cursor': 'begin'})['changes']
control = call('changes_since', {**items, 'cursor': 'begin'})['changes']
assert events(control) == events(changes)
call('batch', {'namespace': ns, 'writes': [
    {'kind': 'insert', 'table': 'items', 'records': [{'slug': 'fresh'}]},
    {'kind': 'insert', 'table': 'items', 'records': [{'slug': 'a'}]},
]}, 'conflict')
assert call('read_rows', items)['rows'] == rows
after_changes = call('changes_since', {**items, 'cursor': 'begin'})['changes']
assert events(after_changes) == events(changes), (changes, after_changes)
print('PASS whole-batch rollback leaves rows and change feed unchanged')
races = {'namespace': ns, 'table': 'races'}
call('create_table', {**races, 'fields': [{'name': 'slug', 'type': 'string'}], 'constraints': [{'name': 'race_key', 'kind': 'unique', 'fields': ['slug']}]})
with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
    results = list(pool.map(lambda _: request('insert', {**races, 'records': [{'slug': 'race'}]}), range(8)))
assert sum(status == 200 and out.get('ok') for status, out in results) == 1, results
for status, out in results:
    assert status == 200 or (status == 400 and out['error']['code'] == 'conflict' and 'race_key' in out['error']['message']), (status, out)
assert len(call('read_rows', races)['rows']) == 1
print('PASS eight concurrent duplicates produce one commit and seven conflicts')
