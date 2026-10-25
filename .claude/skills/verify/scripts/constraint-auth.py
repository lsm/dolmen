import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

base = Path(sys.argv[1], 'url').read_text().strip()
key = os.environ['DOLMEN_ADMIN_KEY']
ns = 'verify_auth'


def call(op, body, principal=None, error=None, mcp=False):
    headers = {'Content-Type': 'application/json'}
    if principal:
        headers['X-Dolmen-Principal'] = principal
    else:
        headers['Authorization'] = 'Bearer ' + key
    if mcp:
        payload = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': op, 'arguments': body}}
    else:
        payload = body
    req = urllib.request.Request(base + ('/mcp' if mcp else '/v1/' + op), json.dumps(payload).encode(), headers)
    try:
        response = urllib.request.urlopen(req, timeout=30)
    except urllib.error.HTTPError as e:
        response = e
    with response:
        out = json.load(response)
    if mcp:
        result = out['result']
        failure = json.loads(result['content'][0]['text']) if result.get('isError') else None
        data = result.get('structuredContent')
    else:
        failure, data = out.get('error'), out.get('data')
    assert (failure or {}).get('code') == error, (op, principal, out)
    return failure if error else data


def grant(principal, table, *verbs):
    obj = {'namespace': ns}
    if table:
        obj['table'] = table
    call('grant', {'subject': {'type': 'principal', 'id': principal}, 'object': obj, 'verbs': verbs})


call('create_namespace', {'namespace': ns})
for table in ('parents', 'spare'):
    call('create_table', {'namespace': ns, 'table': table, 'accept_references': True, 'fields': [{'name': 'slug', 'type': 'string'}]})
    call('insert', {'namespace': ns, 'table': table, 'records': [{'slug': 'p'}]})
call('create_table', {'namespace': ns, 'table': 'children', 'fields': [{'name': 'parent_id', 'type': 'number'}, {'name': 'slug', 'type': 'string'}], 'constraints': [{'name': 'parent_ref', 'kind': 'foreign_key', 'field': 'parent_id', 'references': {'table': 'parents', 'field': 'id'}}, {'name': 'slug_key', 'kind': 'unique', 'fields': ['slug']}]})
print('PASS authenticated constraint declarations')
grant('writer', 'children', 'create')
body = {'namespace': ns, 'table': 'children', 'records': [{'parent_id': 1, 'slug': 'http'}]}
for mcp in (False, True):
    call('insert', body, 'writer', 'forbidden', mcp)
grant('writer', 'children', 'read')
for mcp in (False, True):
    call('insert', body, 'writer', 'forbidden', mcp)
grant('writer', 'parents', 'read')
call('insert', body, 'writer')
body['records'][0]['slug'] = 'mcp'
call('insert', body, 'writer', mcp=True)
print('PASS HTTP/MCP writes require both endpoint reads, then deliver')
messages = []
for table in ('parents', 'spare'):
    grant('remover', table, 'delete', 'read', 'schema', 'admin')
    for mcp in (False, True):
        refused = call('delete', {'namespace': ns, 'table': table, 'filter': 'id = 1'}, 'remover', 'forbidden', mcp)
        messages.append(refused['message'])
assert len(set(messages)) == 1, messages
print('PASS identical parent-delete gate with hidden and zero incoming references')
grant('remover', '', 'read')
call('delete', {'namespace': ns, 'table': 'parents', 'filter': 'id = 1'}, 'remover', 'conflict')
call('delete', {'namespace': ns, 'table': 'spare', 'filter': 'id = 1'}, 'remover')
print('PASS namespace read admits the check; RESTRICT still protects referenced rows')
grant('planner', 'children', 'schema', 'read')
body = {'namespace': ns, 'table': 'children', 'dry_run': True, 'changes': [{'op': 'drop_constraint', 'name': 'parent_ref'}]}
for mcp in (False, True):
    call('migrate', body, 'planner', 'forbidden', mcp)
grant('planner', 'parents', 'read')
for mcp in (False, True):
    call('migrate', body, 'planner', mcp=mcp)
print('PASS HTTP/MCP constraint migrations require every affected table read')
