#!/usr/bin/env python3
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import urllib.request

from jsonschema import Draft202012Validator

work, output = Path(sys.argv[1]), Path(sys.argv[2])
url = (work / 'url').read_text().strip()
env = {k: v for k, v in os.environ.items() if not k.startswith('DOLMEN_') and k not in ('OPENAI_API_KEY', 'REMBED_CACHE', 'HF_TOKEN')}
env['DOLMEN_EMBED_PROVIDER'] = 'none'
schemas = {}
with urllib.request.urlopen(url + '/v1/openapi.json', timeout=15) as response:
    doc = json.load(response)
schemas['OpenAPI'] = doc['paths']['/v1/search_graph']['post']['requestBody']['content']['application/json']['schema']
request = urllib.request.Request(url + '/mcp', json.dumps({'jsonrpc': '2.0', 'id': 1, 'method': 'tools/list', 'params': {}}).encode(), headers={'Content-Type': 'application/json'})
with urllib.request.urlopen(request, timeout=15) as response:
    tools = json.load(response)['result']['tools']
schemas['MCP HTTP'] = next(t['inputSchema'] for t in tools if t['name'] == 'search_graph')

with (work / 'graph-search-schema-stdio.log').open('w') as log:
    proc = subprocess.Popen([str(work / 'dolmen'), 'mcp', '-data', str(work / 'schema-stdio-data')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True, env=env)
    try:
        def rpc(request_id, method, params):
            proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params}) + '\n')
            proc.stdin.flush()
            assert select.select([proc.stdout], [], [], 30)[0], 'stdio timeout'
            reply = json.loads(proc.stdout.readline())
            assert 'error' not in reply, reply
            return reply['result']

        rpc(1, 'initialize', {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'schema-verify', 'version': '1'}})
        tools = rpc(2, 'tools/list', {})['tools']
        schemas['MCP stdio'] = next(t['inputSchema'] for t in tools if t['name'] == 'search_graph')
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()

output.write_text(json.dumps(schemas, indent=2) + '\n')
failures = []
for surface, schema in schemas.items():
    try:
        Draft202012Validator.check_schema(schema)
        Draft202012Validator(schema).validate({'namespace': 'verify', 'tables': ['nodes'], 'vector': [1, 0], 'expansion': {}})
        print(f'PASS compiled {surface}: strict JSON Schema accepts the advertised graph input and optional expansion')
    except Exception as exc:
        failures.append(f'{surface}: {exc}')
        print(f'FAIL compiled {surface}: {exc}')
assert not failures, failures
