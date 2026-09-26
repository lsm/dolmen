# Observing dolmen

Dolmen exports OpenTelemetry traces, metrics and logs over OTLP. This guide is for getting a local
stack up, pointing dolmen at it, and reading one request end to end: the trace that says where the
time went, the metric that says how often it happens, and the log line that says what it was. The
full list of variables, spans and metrics is in the README's
[Observability](../README.md#observability) section; this is the working half of it.

## A local stack

[`grafana/otel-lgtm`](https://github.com/grafana/docker-otel-lgtm) is Tempo, Prometheus, Loki and
Grafana in one image, with OpenSearch-free log shipping and exemplars wired up. It is the quickest
way to see a real trace from a real request.

```yaml
# otel-lgtm.compose.yaml
services:
  lgtm:
    image: grafana/otel-lgtm:latest
    ports:
      - "3000:3000"     # Grafana
      - "4317:4317"     # OTLP gRPC
      - "4318:4318"     # OTLP HTTP
      - "3200:3200"     # Tempo's own API, if you want it
    volumes:
      - lgtm-data:/data

volumes:
  lgtm-data:
```

```bash
docker compose -f otel-lgtm.compose.yaml up -d
```

Traces, metrics and logs are all on by default in that image — it bundles the collector, Tempo,
Prometheus, Loki and Grafana, and needs no feature toggles. The named volume is the path its
components write to, so traces survive `docker compose down`. Open <http://localhost:3000> and log
in as `admin` / `admin` (Grafana's built-in user; change it if the port is reachable by anyone else),
then wait a minute for the dashboards to be provisioned. Nothing in dolmen needs changing beyond
the endpoint: it exports OTLP `http/protobuf` only, and this image accepts it on 4318.

Point dolmen at the stack and turn on all three signals:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_SERVICE_NAME=dolmen-dev \
OTEL_LOGS_EXPORTER=otlp \
./dolmen -data ./data
```

| Variable | Why it is here |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Turns traces **and** metrics on at once. `http://localhost:4318` is the HTTP port; 4317 is gRPC, which dolmen does not speak. |
| `OTEL_SERVICE_NAME` | Names the resource, so the stack can tell one dolmen from another. Defaults to `dolmen`. |
| `OTEL_LOGS_EXPORTER=otlp` | Log lines stay on stderr either way; this additionally ships them, with the request's trace context attached, which is what makes a log line clickable back to its trace. An endpoint alone does not turn this on. |
| `OTEL_METRICS_EXPORTER` | `otlp` (default with an endpoint) or `none`. `prometheus` is refused: scrape `GET /metrics` instead. |
| `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` and unused by default. To sample a tenth of traces on a busy server set **both**: `OTEL_TRACES_SAMPLER=parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG=0.1`. The argument alone changes nothing while the sampler is the `always_on` default. |
| `OTEL_METRIC_EXPORT_INTERVAL` | How often metrics are pushed, in milliseconds. Default `60000`; lower it to `10000` while you are watching a dashboard fill. |

Then make one request. `Content-Type: application/json` is not optional: a body sent as
form-encoded is refused with `415`.

```bash
json='Content-Type: application/json'
curl -s -H "$json" localhost:8790/v1/create_namespace -d '{"namespace":"demo"}'
curl -s -H "$json" localhost:8790/v1/create_table -d '{"namespace":"demo","table":"docs",
  "fields":[{"name":"body","type":"text","vectorize":true}]}'
curl -s -H "$json" localhost:8790/v1/insert -d '{"namespace":"demo","table":"docs",
  "records":[{"body":"the quick brown fox"}]}'
```

Within a minute you should be able to find all three:

- **Traces**: Grafana → Explore → `grafana` data source → *TraceQL* query `{.service.name="dolmen-dev"}`.
  One `insert` request is a tree: `POST /v1/{op}` → `dolmen.op insert` → `INSERT docs`, with
  `embeddings <model>` under it, and on SQLite also `dolmen.writer.wait` and `dolmen.transaction`
  as siblings beside it. Read a slow write from those two: a long `dolmen.writer.wait` means the
  namespace's single writer was busy, and it ends *before* `dolmen.transaction` starts, so the wait
  is contention rather than slow SQL. A long `dolmen.transaction` is the write itself.
- **Metrics**: Grafana → Dashboards → *LGTM Starter Dashboard*, or Explore → `prometheus` data
  source. `dolmen_operation_duration_seconds` is the one to start with; its `count` per
  `dolmen_op_name` is the operation count, and its `sum` divided by that count is the mean.
- **Logs**: Grafana → Explore → `loki` data source. Every line written during a request carries
  `trace_id` and `span_id`, and because the exporter is on, the line links to the trace.

Tear it down with `docker compose -f otel-lgtm.compose.yaml down -v`.

### Pointing a real collector at a real backend

[`otel-collector.yaml`](otel-collector.yaml) is the same thing for teams that already run a
collector: it receives dolmen's traces, metrics and logs over OTLP HTTP and writes them to the
`debug` exporter, with the Tempo/Prometheus/Mimir exporters commented out for you to fill in.

## From a slow metric to the trace that caused it

`dolmen.operation.duration` is a histogram, and when traces are on the OpenTelemetry SDK attaches
**exemplars**: each histogram bucket remembers one recent trace that landed in it. That is the
shortest path from "insert got slow" to "here is what it was waiting on":

1. Open the metric panel and set the time range to the slow window. Switch the panel to the bar or
   heatmap view so the buckets and their exemplars are visible (in Explore, add the
   *Exemplars* data type, or use a heatmap panel).
2. Click the dot on the hottest bucket. Grafana opens the exemplar's trace, already filtered to that
   `service.name`.
3. Read the tree from the outside in: `POST /v1/{op}` (the whole request) → `dolmen.op insert` (the
   operation) → `INSERT docs` (the storage work) → whatever is longest inside it. Each span carries
   its own duration, so the longest child is the answer.
4. If the SDK did not attach exemplars (some backends drop them), filter Explore by
   `{.name="dolmen.op insert" && childCount > 0}` and sort by duration instead. A span with
   `childCount > 0` is an operation, not a leaf, so the one you want is never a bare `embeddings` or
   `INSERT` span.

## Three alerts to start with

These are the three dolmen-specific conditions that are worth waking someone for. Each is written
for Prometheus-compatible query evaluation, which is also what an OpenTelemetry Collector with the
`prometheus` exporter, or Grafana alerting on a recorded rule, evaluates.

**1. Failures: `internal_error` or `timeout` rate.**

```
sum by (dolmen_op_name) (
  rate(dolmen_operation_duration_seconds_count{dolmen_op_outcome=~"internal_error|timeout"}[5m])
)
/
clamp_min(
  sum by (dolmen_op_name) (rate(dolmen_operation_duration_seconds_count[5m])),
  0.001
)
> 0.05
and
sum by (dolmen_op_name) (
  rate(dolmen_operation_duration_seconds_count{dolmen_op_outcome=~"internal_error|timeout"}[5m])
) > 0.1
```

Over five minutes, more than 5% of one operation's calls failing with `internal_error` or
`timeout`, and at least 0.1/s of them, so a single bad request on a quiet server does not page
anyone. Per operation, because `search_vector` failing is a different problem from `insert`
failing. Start with a 30-minute window and page only on `internal_error`; add `timeout` once you
know the baseline.

**2. p99 of a hot operation.** Pick the operation that matters for your workload (usually `insert`
or `query`) and alert on its tail, not its mean:

```
histogram_quantile(0.99,
  sum by (le) (rate(dolmen_operation_duration_seconds_bucket{dolmen_op_name="insert"}[5m]))
) > 1
```

One second for an `insert` is generous for SQLite and tight for PostgreSQL over a network; set it
from the p99 your own data settles on, and add a second rule for `dolmen_op_name="query"` if agents
are latency-sensitive. If the alert fires, the exemplar on the slow bucket is the trace to read, per
the section above.

**3. Embedding failures.** The embedding provider is the one dependency dolmen cannot do without,
and it fails in a way that looks like a bad request:

```
sum(rate(gen_ai_client_operation_duration_seconds_count{error_type!=""}[5m]))
/
clamp_min(sum(rate(gen_ai_client_operation_duration_seconds_count[5m])), 0.001)
> 0.02
```

More than 2% of embedding calls failing over five minutes. The series is split by
`gen_ai_provider_name`, which dolmen reports as `openai` for an OpenAI-compatible endpoint and
`dolmen.local` for the built-in in-process provider, so filter on those values: an `openai` alert can
point at credentials or a quota, and a `dolmen.local` alert at the model cache under
`<data>/models`. Keep the threshold low: an embedding failure fails every write that needs a vector,
so a low-rate version of this matters more than a high-rate version of alert 1.

Two more worth adding once these are quiet, both from the capacity gauges, both engine-specific:
`db_client_connection_max` against `db_client_connection_count` on PostgreSQL (a pool sitting at its
ceiling is what alert 2 looks like before it fires; SQLite has no pool and reports neither series),
and `dolmen_vector_cache_usage_bytes` against `dolmen_vector_cache_limit_bytes` on SQLite (a cache
permanently at its limit is not caching anything new). The `_bytes` suffix is what the OTLP-to-
Prometheus translation adds to the `By` unit, the same way `dolmen_operation_duration_seconds` comes
from `s`; in a backend that keeps the OTel names, query `dolmen.vector_cache.usage` instead. All
five gauges are in the README's metric table.

## What never leaves the process

Worth knowing before you point this at a shared backend, and the rules the code holds to:

- Spans never carry SQL text, query arguments, row values, embedded text, API keys or other
  credentials. Error statuses carry the error code, not the message.
- Metric attributes never carry a namespace, table, principal, request id, path or client address,
  so the series count is bounded by the operation and error-code lists. The exception is opt-in:
  `DOLMEN_OTEL_INCLUDE_PRINCIPAL=true` adds the caller's principal to operation spans as
  `enduser.id`.
- Outbound calls to an `openai` provider carry only `traceparent`; inbound `baggage` is never
  forwarded.
- Log lines are the one place user input can appear (they are your own log lines, at your own log
  level), which is why log export is opt-in even when an endpoint is configured.
