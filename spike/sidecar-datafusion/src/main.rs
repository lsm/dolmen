// Sidecar for DataFusion: reads the spike protocol on stdin, registers one
// namespace's Iceberg table at a pinned snapshot, and runs caller SQL.
//
// See ../driver/PROTOCOL.md for the wire and ../sidecar-duckdb for the DuckDB
// equivalent, since the point of this spike is that both engines are measured
// through the same driver and the same attacks.
//
// datafusion is pinned to the version iceberg-datafusion depends on so the Arrow
// crates in the graph stay one version; the pinning is recorded in Cargo.toml.
use std::env;
use std::io::{self, BufRead, Write};
use std::sync::Arc;

use datafusion::arrow::array::RecordBatch;
use datafusion::arrow::datatypes::SchemaRef;
use datafusion::arrow::util::display::{ArrayFormatter, FormatOptions};
use datafusion::execution::context::{SessionConfig, SessionContext, SQLOptions};
use iceberg::io::FileIO;
use iceberg::spec::{TableMetadata, TableMetadataRef};
use iceberg::table::Table;
use iceberg::TableIdent;
use iceberg_datafusion::IcebergStaticTableProvider;

struct State {
    ctx: SessionContext,
    registered: bool,
    snapshot: i64,
    table: String,
    unlocked: bool,
    running: Option<(String, tokio::task::AbortHandle)>,
}

fn escape(v: &str) -> String {
    let mut out = String::with_capacity(v.len());
    for c in v.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '|' => out.push_str("\\|"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            _ => out.push(c),
        }
    }
    out
}

fn reply(id: &str, payload: &str) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{}\tok\t{}", id, payload);
    let _ = out.flush();
}

fn error_reply(id: &str, class: &str, msg: &str) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{}\terror\t{}\t{}", id, class, escape(msg));
    let _ = out.flush();
}

/// Classify a DataFusion error into the shared subset both sidecars report, so
/// a driver-side test can compare them without knowing which engine answered.
fn classify(msg: &str) -> &'static str {
    let m = msg.to_ascii_lowercase();
    if m.contains("table") && (m.contains("not found") || m.contains("no table")) {
        return "not_found";
    }
    if m.contains("not supported")
        || m.contains("not implemented")
        || m.contains("unsupported")
        || m.contains("error during planning")
        || m.contains("sql error")
        || m.contains("parser error")
        || m.contains("schema error")
        || m.contains("ddl")
        || m.contains("dml")
    {
        return "not_supported";
    }
    "query_error"
}

/// Register the table at the pinned snapshot. iceberg-datafusion's static
/// provider is the read-only, fixed-snapshot one, which is what a namespace
/// pinned to a snapshot needs; the catalog-backed provider refreshes metadata
/// on every scan and would silently move the reader to a newer snapshot.
async fn register(state: &mut State, metadata_path: &str, snapshot: i64, name: &str) -> Result<(), String> {
    let json = std::fs::read_to_string(metadata_path).map_err(|e| format!("read {metadata_path}: {e}"))?;
    let meta: TableMetadata =
        serde_json::from_str(&json).map_err(|e| format!("parse {metadata_path}: {e}"))?;
    let meta_ref: TableMetadataRef = Arc::new(meta);
    if meta_ref.snapshot_by_id(snapshot).is_none() {
        let have: Vec<i64> = meta_ref.snapshots().map(|s| s.snapshot_id()).collect();
        return Err(format!(
            "snapshot {snapshot} is not in {metadata_path}; it holds {have:?}"
        ));
    }
    let io = FileIO::new_with_fs();
    let ident = TableIdent::from_strs(["dolmen", name]).map_err(|e| format!("table ident {name}: {e}"))?;
    let table = Table::builder()
        .identifier(ident)
        .runtime(iceberg::Runtime::current())
        .metadata(meta_ref)
        .metadata_location(metadata_path.to_string())
        .file_io(io)
        .build()
        .map_err(|e| format!("build table: {e}"))?;
    let provider = IcebergStaticTableProvider::try_new_from_table_snapshot(table, snapshot)
        .await
        .map_err(|e| format!("provider for snapshot {snapshot}: {e}"))?;
    state
        .ctx
        .register_table(name, Arc::new(provider))
        .map_err(|e| format!("register {name}: {e}"))?;
    state.registered = true;
    state.snapshot = snapshot;
    state.table = name.to_string();
    Ok(())
}

fn render(cols: &SchemaRef, batches: &[RecordBatch]) -> Result<(String, String), String> {
    let names: Vec<String> = cols
        .fields()
        .iter()
        .map(|f| format!("{}:{}", f.name(), f.data_type()))
        .collect();
    let mut rows = String::new();
    let mut first_row = true;
    for batch in batches {
        for row in 0..batch.num_rows() {
            if !first_row {
                rows.push('|');
            }
            first_row = false;
            for col in 0..batch.num_columns() {
                if col > 0 {
                    rows.push('\u{1f}');
                }
                let fmt = ArrayFormatter::try_new(batch.column(col).as_ref(), &FormatOptions::default())
                    .map_err(|e| e.to_string())?;
                rows.push_str(&escape(&fmt.value(row).to_string()));
            }
        }
    }
    Ok((names.join(";"), rows))
}

/// Run with the options that refuse DDL, DML and multi-statement plans. Which of
/// the confinement properties are these and which are defaults someone could
/// flip is exactly what the driver's confinement tests measure.
async fn run_query(
    ctx: SessionContext,
    unlocked: bool,
    sql: String,
) -> Result<(String, String), String> {
    let opts = if unlocked {
        SQLOptions::new()
    } else {
        SQLOptions::new()
            .with_allow_ddl(false)
            .with_allow_dml(false)
            .with_allow_statements(false)
    };
    let df = ctx
        .sql_with_options(&sql, opts)
        .await
        .map_err(|e| e.to_string())?;
    let schema = df.schema().inner().clone();
    let batches = df.collect().await.map_err(|e| e.to_string())?;
    render(&schema, &batches)
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let data_dir = env::var("SIDECAR_DATA_DIR").unwrap_or_default();
    if data_dir.is_empty() {
        eprintln!("sidecar: SIDECAR_DATA_DIR is required");
        std::process::exit(2);
    }
    let memory_max = env::var("SIDECAR_MEMORY_MAX")
        .ok()
        .map(|v| parse_bytes(&v))
        .unwrap_or(0);

    let config = SessionConfig::new().with_coalesce_batches(true);
    let mut builder = datafusion::execution::runtime_env::RuntimeEnvBuilder::new();
    if memory_max > 0 {
        builder = builder.with_memory_pool(Arc::new(
            datafusion::execution::memory_pool::FairSpillPool::new(memory_max),
        ));
    }
    let runtime = builder.build_arc()?;
    let ctx = SessionContext::new_with_config_rt(config, runtime);

    let mut state = State {
        ctx,
        registered: false,
        snapshot: -1,
        table: String::new(),
        unlocked: env::var("SIDECAR_UNLOCKED").ok().as_deref() == Some("1"),
        running: None,
    };

    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel::<String>();
    std::thread::spawn(move || {
        let stdin = io::stdin();
        for line in stdin.lock().lines() {
            match line {
                Ok(l) => {
                    if tx.send(l).is_err() {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
    });
    let (done_tx, mut done_rx) = tokio::sync::mpsc::unbounded_channel::<String>();

    loop {
        let line = tokio::select! {
            Some(id) = done_rx.recv() => {
                if state.running.as_ref().map(|(r, _)| r == &id).unwrap_or(false) {
                    state.running = None;
                }
                continue;
            }
            line = rx.recv() => match line {
                Some(l) => l,
                None => break,
            },
        };
        let f: Vec<&str> = line.split('\t').collect();
        if f.len() < 2 {
            continue;
        }
        let id = f[0].to_string();
        match f[1] {
            "shutdown" => {
                if let Some((_, handle)) = state.running.take() {
                    handle.abort();
                }
                reply(&id, "");
                break;
            }
            "cancel" => {
                if let Some((qid, handle)) = state.running.take() {
                    handle.abort();
                    error_reply(&qid, "canceled", "query was cancelled");
                }
                if id != "0" {
                    reply(&id, "");
                }
            }
            "memory_limit" => reply(&id, ""),
            "init" => {
                if f.len() < 6 {
                    error_reply(&id, "internal_error", "init needs dataDir, snapshot, table and location");
                    continue;
                }
                let snapshot: i64 = f[3].parse().unwrap_or(-1);
                let table = f[4].to_string();
                let dir = format!("{}/{}/metadata", data_dir, f[5]);
                match latest_metadata(&dir).await {
                    Ok(path) => match register(&mut state, &path, snapshot, &table).await {
                        Ok(()) => {
                            eprintln!("sidecar: registered {table} at {snapshot} from {path}, registered={}", state.registered);
                            reply(&id, &format!("registered {} at {}", table, snapshot))
                        }
                        Err(e) => {
                            eprintln!("sidecar: init refused: {e}");
                            error_reply(&id, "not_found", &e)
                        }
                    },
                    Err(e) => {
                        eprintln!("sidecar: init found no metadata: {e}");
                        error_reply(&id, "not_found", &e)
                    }
                }
            }
            "query" => {
                if f.len() < 3 {
                    error_reply(&id, "internal_error", "query needs sql");
                    continue;
                }
                if !state.registered {
                    eprintln!("sidecar: query {id} arrived with nothing registered");
                    error_reply(&id, "internal_error", "no table registered; send init first");
                    continue;
                }
                if state.running.is_some() {
                    error_reply(&id, "internal_error", "a query is already running");
                    continue;
                }
                let sql = f[2].to_string();
                let ctx = state.ctx.clone();
                let unlocked = state.unlocked;
                let qid = id.clone();
                let done = done_tx.clone();
                let task = tokio::spawn(async move {
                    let start = std::time::Instant::now();
                    match run_query(ctx, unlocked, sql).await {
                        Ok((cols, rows)) => {
                            let elapsed = start.elapsed().as_millis() as i64;
                            reply(&qid, &format!("{cols}\t{rows}\t{elapsed}\t0"));
                        }
                        Err(e) => error_reply(&qid, classify(&e), &e),
                    }
                    let _ = done.send(qid);
                });
                state.running = Some((id, task.abort_handle()));
            }
            other => error_reply(&id, "internal_error", &format!("unknown op {other}")),
        }
    }
    Ok(())
}

fn parse_bytes(v: &str) -> usize {
    let mut bytes: usize = 0;
    for c in v.chars() {
        match c {
            '0'..='9' => bytes = bytes * 10 + (c as usize - '0' as usize),
            'K' | 'k' => bytes *= 1024,
            'M' | 'm' => bytes *= 1024 * 1024,
            'G' | 'g' => bytes *= 1024 * 1024 * 1024,
            _ => {}
        }
    }
    bytes
}

/// The newest metadata JSON in a directory. Iceberg writes one per commit and
/// names them by version, so the newest file is the current table state; the
/// pinned snapshot id in `init` is what actually decides which data is read.
async fn latest_metadata(dir: &str) -> Result<String, String> {
    let mut entries = tokio::fs::read_dir(dir)
        .await
        .map_err(|e| format!("read {dir}: {e}"))?;
    let mut best: Option<(std::time::SystemTime, String)> = None;
    while let Some(entry) = entries
        .next_entry()
        .await
        .map_err(|e| e.to_string())?
    {
        let name = entry.file_name().to_string_lossy().to_string();
        if !name.ends_with(".metadata.json") {
            continue;
        }
        let md = entry.metadata().await.map_err(|e| e.to_string())?;
        let t = md.modified().map_err(|e| e.to_string())?;
        if best.as_ref().map(|(bt, _)| t > *bt).unwrap_or(true) {
            best = Some((t, entry.path().to_string_lossy().to_string()));
        }
    }
    best.map(|(_, p)| p)
        .ok_or_else(|| format!("no .metadata.json under {dir}"))
}
