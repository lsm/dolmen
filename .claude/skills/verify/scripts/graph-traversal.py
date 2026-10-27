#!/usr/bin/env python3
import json
import os
from pathlib import Path
import select
import sqlite3
import subprocess
import sys
import urllib.error
import urllib.request

work, output = Path(sys.argv[1]), Path(sys.argv[2])
url = (work / 'url').read_text().strip()
env = {k: v for k, v in os.environ.items() if not k.startswith('DOLMEN_') and k not in ('OPENAI_API_KEY', 'REMBED_CACHE', 'HF_TOKEN')}
env['DOLMEN_EMBED_PROVIDER'] = 'none'
evidence = {}
proc = None
request_id = 0


def rpc(method, params):
    global request_id
    request_id += 1
    proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params}) + '\n')
    proc.stdin.flush()
    if not select.select([proc.stdout], [], [], 30)[0]:
        raise RuntimeError('MCP stdio response timed out')
    reply = json.loads(proc.stdout.readline())
    assert reply.get('id') == request_id and 'error' not in reply, reply
    return reply['result']


def call(surface, op, body):
    if surface == 'stdio':
        result = rpc('tools/call', {'name': op, 'arguments': body})
        assert not result.get('isError'), result
        return result['structuredContent']
    mcp = surface == 'mcp'
    payload = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': op, 'arguments': body}} if mcp else body
    req = urllib.request.Request(url + ('/mcp' if mcp else '/v1/' + op), json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=30) as response:
        reply = json.load(response)
    if mcp:
        assert 'error' not in reply and not reply['result'].get('isError'), reply
        return reply['result']['structuredContent']
    assert reply.get('ok'), reply
    return reply['data']


def seed(surface, ns):
    call(surface, 'create_namespace', {'namespace': ns})
    call(surface, 'create_table', {'namespace': ns, 'table': 'nodes', 'accept_references': True, 'fields': [{'name': 'label', 'type': 'string'}]})
    call(surface, 'create_table', {'namespace': ns, 'table': 'links', 'fields': [{'name': f, 'type': typ, 'required': True} for f, typ in [('src', 'number'), ('dst', 'number'), ('kind', 'string')]], 'constraints': [{'name': f + '_ref', 'kind': 'foreign_key', 'field': f, 'references': {'table': 'nodes', 'field': 'id'}} for f in ('src', 'dst')], 'relation': {'source': 'src', 'target': 'dst', 'type': 'kind'}})
    call(surface, 'insert', {'namespace': ns, 'table': 'nodes', 'records': [{'label': str(i)} for i in range(1, 5)]})
    for source, target in [(1, 2), (1, 4), (2, 3), (4, 3)]:
        call(surface, 'relate', {'namespace': ns, 'table': 'links', 'source': source, 'target': target, 'type': 'k'})


with (work / 'graph-traversal-stdio.log').open('w') as log:
    proc = subprocess.Popen([str(work / 'dolmen'), 'mcp', '-data', str(work / 'traversal-stdio-data')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True, env=env)
    try:
        rpc('initialize', {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'graph-verify', 'version': '1'}})
        proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'method': 'notifications/initialized'}) + '\n')
        proc.stdin.flush()
        for surface in ('http', 'mcp', 'stdio'):
            ns = 'traverse_' + surface
            seed(surface, ns)
            ref = lambda n: {'table': 'nodes', 'id': n}
            for op in ('find_path', 'subgraph'):
                body = {'namespace': ns, 'direction': 'out'}
                body.update(source=ref(1), target=ref(3)) if op == 'find_path' else body.update(seeds=[ref(1), ref(1)])
                out = call(surface, op, body)
                assert not out['truncated'] and out['stop_reason'] == 'complete', out
                assert [r['id'] for r in out['nodes']] == ([1, 2, 3] if op == 'find_path' else [1, 2, 3, 4]), out
                assert len(out['edges']) == (2 if op == 'find_path' else 4), out
                evidence[f'{surface}_{op}_complete'] = out
                for field, reason in [('max_hops', 'hops'), ('max_nodes', 'nodes'), ('max_edges', 'edges'), ('max_work', 'work')]:
                    bounded = call(surface, op, {**body, field: 1})
                    assert bounded['truncated'] and bounded['stop_reason'] == reason, bounded
                    if op == 'find_path':
                        assert bounded['nodes'] == [] and bounded['edges'] == [], bounded
                    evidence[f'{surface}_{op}_{reason}'] = bounded
            print(f'PASS compiled {surface}: shortest path, deduplicated subgraph, hops/nodes/edges/work stops')
        for surface in ('http', 'mcp', 'stdio'):
            ns = 'elapsed_' + surface
            seed(surface, ns)
            data = work / ('traversal-stdio-data' if surface == 'stdio' else 'data')
            with sqlite3.connect(data / (ns + '.db'), timeout=1) as lock:
                lock.execute('BEGIN IMMEDIATE')
                for op in ('find_path', 'subgraph'):
                    body = {'namespace': ns, 'max_elapsed_ms': 1}
                    body.update(source={'table': 'nodes', 'id': 1}, target={'table': 'nodes', 'id': 3}) if op == 'find_path' else body.update(seeds=[{'table': 'nodes', 'id': 1}])
                    out = call(surface, op, body)
                    assert out['truncated'] and out['stop_reason'] == 'elapsed', out
                    evidence[f'{surface}_{op}_elapsed'] = out
                lock.rollback()
            print(f'PASS compiled {surface}: work deadline includes blocked snapshot/index setup')
    finally:
        proc.stdin.close()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        evidence['stdio_closed'] = proc.poll() is not None
        output.write_text(json.dumps(evidence, indent=2) + '\n')
print('PASS synthetic MCP stdio process stopped')
