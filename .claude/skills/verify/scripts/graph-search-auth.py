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

work, output = Path(sys.argv[1]), Path(sys.argv[2])
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
url = f'http://127.0.0.1:{port}'
admin = secrets.token_urlsafe(32)
env = {k: v for k, v in os.environ.items() if not k.startswith('DOLMEN_') and k not in ('OPENAI_API_KEY', 'REMBED_CACHE', 'HF_TOKEN')}
env.update(DOLMEN_ADMIN_KEY=admin, DOLMEN_EMBED_PROVIDER='none')
evidence = {}


def call(op, body, key=admin, mcp=False):
    payload = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': op, 'arguments': body}} if mcp else body
    req = urllib.request.Request(url + ('/mcp' if mcp else '/v1/' + op), json.dumps(payload).encode(), headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + key})
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            reply = json.load(response)
    except urllib.error.HTTPError as exc:
        reply = json.load(exc)
    if mcp:
        assert 'error' not in reply, reply
        result = reply['result']
        if result.get('isError'):
            error = json.loads(result['content'][0]['text'])
            return error, error['code']
        return result['structuredContent'], ''
    return (reply['data'], '') if reply.get('ok') else (reply['error'], reply['error']['code'])


def must(op, body, key=admin):
    data, code = call(op, body, key)
    assert not code, data
    return data


def grant(principal, table, verbs):
    must('grant', {'subject': {'type': 'principal', 'id': principal}, 'object': {'namespace': 'verify', 'table': table}, 'verbs': verbs})


with (work / 'graph-search-auth.log').open('w') as log:
    proc = subprocess.Popen([str(work / 'dolmen'), '-addr', f'127.0.0.1:{port}', '-data', str(work / 'graph-search-auth-data'), '-auth', 'on'], env=env, stdout=log, stderr=log)
    try:
        deadline = time.monotonic() + 15
        while True:
            try:
                with urllib.request.urlopen(url + '/healthz', timeout=1):
                    break
            except (OSError, urllib.error.URLError):
                if proc.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError('synthetic auth daemon did not start; inspect graph-search-auth.log')
                time.sleep(0.1)
        must('create_namespace', {'namespace': 'verify'})
        keys = {p: must('create_key', {'name': p, 'principal': p})['secret'] for p in ('reader', 'alice', 'bob')}
        for table, dim in [('visible', 2), ('hidden', 3), ('own_nodes', 2)]:
            body = {'namespace': 'verify', 'table': table, 'fields': [{'name': 'v', 'type': 'vector', 'dim': dim}]}
            if table == 'own_nodes':
                body['row_access'] = 'own'
            must('create_table', body)
        grant('reader', 'visible', ['read'])
        for mcp in (False, True):
            for table in ('hidden', 'absent'):
                data, code = call('search_graph', {'namespace': 'verify', 'tables': [table], 'vector': [1, 0]}, keys['reader'], mcp)
                evidence[f'forbidden_{mcp}_{table}'] = data
                assert code == 'forbidden', (table, code, data)
            print(f'PASS compiled {"MCP" if mcp else "HTTP"}: unreadable and absent seed tables both return forbidden')
        for mcp in (False, True):
            oversized = ['not-a-number'] + [0] * 4096
            data, code = call('search_graph', {'namespace': 'verify', 'tables': ['visible'], 'vector': oversized}, keys['reader'], mcp)
            evidence[f'vector_bound_{mcp}'] = data
            assert code == 'invalid_request' and 'vector has more than 4096 numbers' in data['message'], (code, data)
            print(f'PASS compiled {"MCP" if mcp else "HTTP"}: vector bound precedes typed element decoding')
        for principal in ('alice', 'bob'):
            grant(principal, '', ['read', 'create'])
            must('insert', {'namespace': 'verify', 'table': 'own_nodes', 'records': [{'v': [1, 0]}]}, keys[principal])
        must('revoke', {'subject': {'type': 'principal', 'id': 'alice'}, 'object': {'namespace': 'verify'}, 'verbs': ['read', 'create']})
        grant('alice', 'own_nodes', ['update'])
        for mcp in (False, True):
            data, code = call('search_graph', {'namespace': 'verify', 'tables': ['own_nodes'], 'vector': [1, 0]}, keys['alice'], mcp)
            assert not code and [s['ref']['id'] for s in data['seeds']] == [1] and len(data['nodes']) == 1, (code, data)
            evidence[f'owner_{mcp}'] = data
            print(f'PASS compiled {"MCP" if mcp else "HTTP"}: own-row update grant searches only its owner\'s seed')
    finally:
        output.write_text(json.dumps(evidence, indent=2) + '\n')
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
