#!/usr/bin/env python3
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

work = Path(sys.argv[1])
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
url = f'http://127.0.0.1:{port}'
admin = secrets.token_urlsafe(32)
env = {k: v for k, v in os.environ.items() if not k.startswith('DOLMEN_') and k not in ('OPENAI_API_KEY', 'REMBED_CACHE', 'HF_TOKEN')}
env.update(DOLMEN_ADMIN_KEY=admin, DOLMEN_EMBED_PROVIDER='none')
evidence = {}


def call(op, body, key=admin, mcp=False):
    path = '/mcp' if mcp else '/v1/' + op
    if mcp:
        body = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': op, 'arguments': body}}
    req = urllib.request.Request(url + path, json.dumps(body).encode(), headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + key})
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            out = json.load(response)
    except urllib.error.HTTPError as exc:
        out = json.load(exc)
    if mcp:
        assert 'error' not in out, out
        result = out['result']
        if result.get('isError'):
            return result
        return result['structuredContent']
    assert out.get('ok'), out
    return out['data']


def grant(principal, table, verbs):
    return call('grant', {'subject': {'type': 'principal', 'id': principal}, 'object': {'namespace': 'verify', 'table': table}, 'verbs': verbs})


def declare(table, parents=(), own=False):
    body = {'namespace': 'verify', 'table': table, 'fields': [{'name': 'label', 'type': 'string'}], 'accept_references': True}
    if own:
        body['row_access'] = 'own'
    if parents:
        body.pop('accept_references')
        body.update(fields=[{'name': f, 'type': t, 'required': True} for f, t in [('src', 'number'), ('dst', 'number'), ('kind', 'string')]], constraints=[{'name': f + '_ref', 'kind': 'foreign_key', 'field': f, 'references': {'table': p, 'field': 'id'}} for f, p in zip(('src', 'dst'), parents)], relation={'source': 'src', 'target': 'dst', 'type': 'kind'})
    call('create_table', body)


def neighbors(table, key, mcp=False, **opts):
    return call('neighbors', {'namespace': 'verify', 'node': {'table': table, 'id': 1}, **opts}, key, mcp)


with (work / 'graph-auth.log').open('w') as log:
    proc = subprocess.Popen([str(work / 'dolmen'), '-addr', f'127.0.0.1:{port}', '-data', str(work / 'graph-auth-data'), '-auth', 'on'], env=env, stdout=log, stderr=log)
    try:
        deadline = time.monotonic() + 15
        while True:
            try:
                with urllib.request.urlopen(url + '/healthz', timeout=1) as response:
                    assert response.status == 200
                break
            except (OSError, urllib.error.URLError):
                if proc.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError('synthetic graph daemon did not become healthy; inspect graph-auth.log')
                time.sleep(0.1)
        call('create_namespace', {'namespace': 'verify'})
        keys = {p: call('create_key', {'name': p, 'principal': p})['secret'] for p in ('reader', 'alice', 'bob')}
        for table in ('nodes', 'private_nodes'):
            declare(table)
            call('insert', {'namespace': 'verify', 'table': table, 'records': [{'label': 'synthetic'}]})
        declare('links', ('nodes', 'private_nodes'))
        call('relate', {'namespace': 'verify', 'table': 'links', 'source': 1, 'target': 1, 'type': 'knows'})
        for table in ('nodes', 'links'):
            grant('reader', table, ['read'])
        grant('reader', 'private_nodes', ['update'])
        for mcp in (False, True):
            out = neighbors('nodes', keys['reader'], mcp)
            assert len(out['nodes']) == 1 and out['edges'] == [] and not out['truncated'], out
            evidence[f'hidden_endpoint_{mcp}'] = out
        print('PASS compiled daemon: HTTP and MCP suppress an unreadable endpoint despite its update grant')
        grant('reader', 'private_nodes', ['read'])
        for mcp in (False, True):
            out = neighbors('nodes', keys['reader'], mcp)
            assert len(out['nodes']) == 2 and len(out['edges']) == 1 and out['stop_reason'] == 'complete', out
            bounded = neighbors('nodes', keys['reader'], mcp, max_nodes=1)
            assert bounded['truncated'] and bounded['stop_reason'] == 'nodes' and bounded['edges'] == [], bounded
            evidence[f'visible_{mcp}'], evidence[f'bounded_{mcp}'] = out, bounded
        print('PASS compiled daemon: HTTP and MCP return stable references and explicit budget truncation')
        declare('own_nodes', own=True)
        declare('own_links', ('own_nodes', 'own_nodes'), own=True)
        for principal in ('alice', 'bob'):
            grant(principal, '', ['read', 'create'])
            inserted = call('insert', {'namespace': 'verify', 'table': 'own_nodes', 'records': [{'label': principal}]}, keys[principal])
            node = inserted['ids'][0]
            call('relate', {'namespace': 'verify', 'table': 'own_links', 'source': node, 'target': node, 'type': 'self'}, keys[principal])
        call('revoke', {'subject': {'type': 'principal', 'id': 'alice'}, 'object': {'namespace': 'verify'}, 'verbs': ['read', 'create']})
        for table in ('own_nodes', 'own_links'):
            grant('alice', table, ['update'])
        for mcp in (False, True):
            out = neighbors('own_nodes', keys['alice'], mcp)
            assert len(out['nodes']) == 1 and len(out['edges']) == 1, out
            hidden = call('neighbors', {'namespace': 'verify', 'node': {'table': 'own_nodes', 'id': 2}}, keys['alice'], mcp)
            assert hidden['nodes'] == [] and hidden['edges'] == [], hidden
            evidence[f'owner_{mcp}'], evidence[f'hidden_seed_{mcp}'] = out, hidden
        print('PASS compiled daemon: real API keys and owner scopes hide other owners\' nodes and edges')
        Path(sys.argv[2]).write_text(json.dumps(evidence, indent=2) + '\n')
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
print('PASS synthetic auth daemon stopped')
