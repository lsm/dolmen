#!/usr/bin/env python3
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import urllib.request

work, output = Path(sys.argv[1]), Path(sys.argv[2])
url = (work / 'url').read_text().strip()
env = {k: v for k, v in os.environ.items() if not k.startswith('DOLMEN_') and k not in ('OPENAI_API_KEY', 'REMBED_CACHE', 'HF_TOKEN')}
env['DOLMEN_EMBED_PROVIDER'] = 'none'
evidence = {}
request_id = 0


def rpc(method, params):
    global request_id
    request_id += 1
    proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params}) + '\n')
    proc.stdin.flush()
    assert select.select([proc.stdout], [], [], 30)[0], 'stdio timeout'
    reply = json.loads(proc.stdout.readline())
    assert 'error' not in reply, reply
    return reply['result']


def call(surface, op, body):
    if surface == 'stdio':
        reply = rpc('tools/call', {'name': op, 'arguments': body})
        assert not reply.get('isError'), reply
        return reply['structuredContent']
    payload = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': op, 'arguments': body}} if surface == 'mcp' else body
    req = urllib.request.Request(url + ('/mcp' if surface == 'mcp' else '/v1/' + op), json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=30) as response:
        reply = json.load(response)
    if surface == 'mcp':
        assert 'error' not in reply and not reply['result'].get('isError'), reply
        return reply['result']['structuredContent']
    assert reply.get('ok'), reply
    return reply['data']


with (work / 'graph-search-stdio.log').open('w') as log:
    proc = subprocess.Popen([str(work / 'dolmen'), 'mcp', '-data', str(work / 'search-stdio-data')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True, env=env)
    try:
        rpc('initialize', {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'search-verify', 'version': '1'}})
        for surface in ('http', 'mcp', 'stdio'):
            ns = 'search_' + surface
            call(surface, 'create_namespace', {'namespace': ns})
            for table in ('nodes', 'other'):
                call(surface, 'create_table', {'namespace': ns, 'table': table, 'accept_references': True, 'fields': [{'name': 'v', 'type': 'vector', 'dim': 2}, {'name': 'label', 'type': 'string'}]})
                call(surface, 'insert', {'namespace': ns, 'table': table, 'records': [{'label': 'keep', 'v': [1, 0]}, {'label': 'drop', 'v': [0, 1]}]})
            call(surface, 'create_table', {'namespace': ns, 'table': 'links', 'fields': [{'name': f, 'type': t, 'required': True} for f, t in [('src', 'number'), ('dst', 'number'), ('kind', 'string')]], 'constraints': [{'name': f + '_ref', 'kind': 'foreign_key', 'field': f, 'references': {'table': 'nodes', 'field': 'id'}} for f in ('src', 'dst')], 'relation': {'source': 'src', 'target': 'dst', 'type': 'kind'}})
            call(surface, 'relate', {'namespace': ns, 'table': 'links', 'source': 1, 'target': 2, 'type': 'knows'})
            body = {'namespace': ns, 'tables': ['other', 'nodes'], 'vector': [1, 0], 'top_k': 1, 'filter': 'label = ?', 'args': ['keep'], 'expansion': {'relation_types': ['knows']}}
            out = call(surface, 'search_graph', body)
            assert out['seeds'] == [{'ref': {'table': 'nodes', 'id': 1}, 'score': 1}], out
            assert len(out['nodes']) == 2 and len(out['edges']) == 1 and not out['truncated'], out
            evidence[surface + '_complete'] = out
            for expansion, reason in [({'max_nodes': 1}, 'nodes'), ({'max_hops': 1}, 'hops')]:
                out = call(surface, 'search_graph', {**body, 'expansion': expansion})
                assert out['truncated'] and out['stop_reason'] == reason, out
                evidence[surface + '_' + reason] = out
            out = call(surface, 'search_graph', {**body, 'expansion': {'relation_types': ['other']}})
            assert len(out['nodes']) == 1 and not out['edges'] and not out['truncated'], out
            out = call(surface, 'search_graph', {**body, 'args': ['absent']})
            assert out['seeds'] == [] and out['nodes'] == [] and not out['truncated'], out
            print(f'PASS compiled {surface}: global seed ranking, scores, property/type filters, expansion, node/hop bounds, empty search')
    finally:
        proc.terminate()
        proc.wait(timeout=10)
output.write_text(json.dumps(evidence, indent=2) + '\n')
