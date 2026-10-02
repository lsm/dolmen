// Sidecar for DuckDB: reads the spike protocol on stdin, registers one
// namespace's Iceberg tables at a pinned snapshot, and runs caller SQL.
//
// Built against prebuilt libduckdb with the iceberg and httpfs extensions
// pre-placed in the extension directory, so nothing downloads at runtime. See
// ../driver/PROTOCOL.md for the wire.
//
// Lifecycle, in three phases:
//
//   1. init ops only record tables; no caller SQL can run yet.
//   2. The first query seals the sidecar. A private catalog file is written
//      read-write with one view per table, closed, and reopened READ_ONLY with
//      the confinement applied. No caller SQL has run against the writable copy.
//   3. Queries run on a worker thread against one connection, so the main thread
//      keeps reading stdin and a cancel reaches Connection::Interrupt mid-flight.
#include "duckdb.hpp"

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <filesystem>
#include <random>
#include <iostream>
#include <memory>
#include <mutex>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

namespace {

struct TableSpec {
  std::string name;
  std::string metadata;
  long long snapshot;
};

std::mutex g_out_mu;
std::vector<TableSpec> g_tables;
std::unique_ptr<duckdb::DuckDB> g_db;
std::unique_ptr<duckdb::Connection> g_con;
std::string g_catalog_path;
bool g_sealed = false;

std::mutex g_work_mu;
std::condition_variable g_work_cv;
std::string g_work_id;
std::string g_work_sql;
bool g_work_ready = false;
std::atomic<bool> g_busy{false};
bool g_stop = false;

std::vector<std::string> Split(const std::string &line, char sep) {
  std::vector<std::string> out;
  std::string cur;
  for (char c : line) {
    if (c == sep) {
      out.push_back(cur);
      cur.clear();
    } else {
      cur.push_back(c);
    }
  }
  out.push_back(cur);
  return out;
}

std::string Escape(const std::string &v) {
  std::string out;
  for (char c : v) {
    if (c == '\\') {
      out += "\\\\";
    } else if (c == '|') {
      out += "\\|";
    } else if (c == '\n') {
      out += "\\n";
    } else if (c == '\t') {
      out += "\\t";
    } else {
      out.push_back(c);
    }
  }
  return out;
}

void Emit(const std::string &line) {
  std::lock_guard<std::mutex> lock(g_out_mu);
  std::cout << line << std::endl;
}

void Reply(const std::string &id, const std::string &payload) { Emit(id + "\tok\t" + payload); }

void ErrorReply(const std::string &id, const std::string &cls, const std::string &msg) {
  Emit(id + "\terror\t" + cls + "\t" + Escape(msg));
}

std::string Quote(const std::string &v) {
  std::string out = "'";
  for (char c : v) {
    if (c == '\'') out += "''";
    else out.push_back(c);
  }
  return out + "'";
}

std::string Ident(const std::string &v) {
  std::string out = "\"";
  for (char c : v) {
    if (c == '"') out += "\"\"";
    else out.push_back(c);
  }
  return out + "\"";
}

// The newest .metadata.json under dir. Iceberg writes one per commit, so the
// newest is the current table state; the snapshot id decides which data is read.
std::string LatestMetadata(const std::string &dir) {
  namespace fs = std::filesystem;
  std::error_code ec;
  std::string best;
  fs::file_time_type best_time{};
  for (const auto &entry : fs::directory_iterator(dir, ec)) {
    if (ec) return "";
    const std::string name = entry.path().filename().string();
    if (name.size() < 14 || name.compare(name.size() - 14, 14, ".metadata.json") != 0) continue;
    const auto t = entry.last_write_time(ec);
    if (ec) continue;
    if (best.empty() || t > best_time) {
      best = entry.path().string();
      best_time = t;
    }
  }
  return best;
}

bool Exec(duckdb::Connection &con, const std::string &sql, std::string &err);

bool HasSnapshot(const std::string &ext_dir, const std::string &metadata, long long snapshot,
                 std::string &err) {
  duckdb::DBConfig cfg;
  cfg.options.log_config.enabled = false;
  duckdb::DuckDB probe(nullptr, &cfg);
  duckdb::Connection con(probe);
  if (!ext_dir.empty() && !Exec(con, "SET extension_directory = " + Quote(ext_dir), err)) return false;
  if (!Exec(con, "LOAD iceberg", err)) return false;
  auto res = con.Query("SELECT count(*) FROM iceberg_snapshots(" + Quote(metadata) +
                       ") WHERE snapshot_id = " + std::to_string(snapshot));
  if (res->HasError()) {
    err = res->GetError();
    return false;
  }
  if (res->GetValue(0, 0).GetValue<int64_t>() == 0) {
    err = "snapshot " + std::to_string(snapshot) + " is not in " + metadata;
    return false;
  }
  return true;
}

std::string Classify(const std::string &msg) {
  std::string lower = msg;
  for (auto &c : lower) c = static_cast<char>(tolower(c));
  if (lower.find("interrupted") != std::string::npos) return "canceled";
  if (lower.find("does not exist") != std::string::npos ||
      lower.find("no files found") != std::string::npos ||
      lower.find("catalog error") != std::string::npos) {
    return "not_found";
  }
  if (lower.find("not implemented") != std::string::npos ||
      lower.find("disabled") != std::string::npos ||
      lower.find("not supported") != std::string::npos ||
      lower.find("read-only") != std::string::npos ||
      lower.find("read only") != std::string::npos ||
      lower.find("permission error") != std::string::npos ||
      lower.find("parser error") != std::string::npos ||
      lower.find("no function matches") != std::string::npos ||
      lower.find("binder error") != std::string::npos ||
      lower.find("cannot be loaded") != std::string::npos ||
      lower.find("not allowed") != std::string::npos) {
    return "not_supported";
  }
  return "query_error";
}

void ResultReply(const std::string &id, duckdb::MaterializedQueryResult &res, long long elapsed) {
  std::ostringstream cols;
  for (duckdb::idx_t i = 0; i < res.ColumnCount(); i++) {
    if (i) cols << ";";
    cols << res.ColumnName(i) << ":" << res.types[i].ToString();
  }
  std::ostringstream rows;
  for (duckdb::idx_t i = 0; i < res.RowCount(); i++) {
    if (i) rows << "|";
    for (duckdb::idx_t c = 0; c < res.ColumnCount(); c++) {
      if (c) rows << "\x1f";
      rows << Escape(res.GetValue(c, i).ToString());
    }
  }
  Emit(id + "\tok\t" + cols.str() + "\t" + rows.str() + "\t" + std::to_string(elapsed) + "\t0");
}

duckdb::idx_t ParseBytes(const char *v) {
  duckdb::idx_t bytes = 0;
  for (const char *p = v; *p != '\0'; p++) {
    if (*p >= '0' && *p <= '9') {
      bytes = bytes * 10 + static_cast<duckdb::idx_t>(*p - '0');
    } else if (*p == 'K' || *p == 'k') {
      bytes *= 1024ULL;
    } else if (*p == 'M' || *p == 'm') {
      bytes *= 1024ULL * 1024ULL;
    } else if (*p == 'G' || *p == 'g') {
      bytes *= 1024ULL * 1024ULL * 1024ULL;
    }
  }
  return bytes;
}

bool Exec(duckdb::Connection &con, const std::string &sql, std::string &err) {
  auto res = con.Query(sql);
  if (res->HasError()) {
    err = sql + " -> " + res->GetError();
    return false;
  }
  return true;
}

// Phase 2. The writable copy only ever runs statements this function builds;
// caller SQL first runs after the READ_ONLY reopen and the lock.
bool Seal(const std::string &data_dir, const std::string &ext_dir, bool unlocked, std::string &err) {
  namespace fs = std::filesystem;
  g_catalog_path = (fs::temp_directory_path() /
                    ("dolmen-sidecar-" + std::to_string(std::random_device{}()) + "-" +
                     std::to_string(std::chrono::steady_clock::now().time_since_epoch().count()) +
                     ".duckdb"))
                       .string();
  std::error_code ec;
  fs::remove(g_catalog_path, ec);
  fs::remove(g_catalog_path + ".wal", ec);
  {
    duckdb::DBConfig seed_cfg;
    seed_cfg.options.log_config.enabled = false;
    duckdb::DuckDB seed(g_catalog_path.c_str(), &seed_cfg);
    duckdb::Connection con(seed);
    if (!ext_dir.empty() && !Exec(con, "SET extension_directory = " + Quote(ext_dir), err)) return false;
    if (!Exec(con, "LOAD iceberg", err)) return false;
    for (const auto &t : g_tables) {
      const std::string sql = "CREATE VIEW " + Ident(t.name) + " AS SELECT * FROM iceberg_scan(" +
                              Quote(t.metadata) + ", snapshot_from_id => " +
                              std::to_string(t.snapshot) + ")";
      if (!Exec(con, sql, err)) return false;
    }
    if (!Exec(con, "CHECKPOINT", err)) return false;
  }

  duckdb::DBConfig cfg;
  cfg.options.log_config.enabled = false;
  const char *mem_env = std::getenv("SIDECAR_MEMORY_MAX");
  if (mem_env != nullptr && *mem_env != '\0') cfg.options.maximum_memory = ParseBytes(mem_env);
  if (!unlocked) {
    cfg.options.access_mode = duckdb::AccessMode::READ_ONLY;
    cfg.options.allowed_directories.insert(data_dir);
  }
  g_db = std::make_unique<duckdb::DuckDB>(g_catalog_path.c_str(), &cfg);
  g_con = std::make_unique<duckdb::Connection>(*g_db);

  if (!ext_dir.empty() && !Exec(*g_con, "SET extension_directory = " + Quote(ext_dir), err)) return false;
  if (!Exec(*g_con, "LOAD iceberg", err)) return false;
  if (!unlocked) {
    for (const char *stmt : {"SET enable_external_access = false",
                             "SET autoinstall_known_extensions = false",
                             "SET autoload_known_extensions = false",
                             "SET allow_persistent_secrets = false",
                             "SET lock_configuration = true"}) {
      if (!Exec(*g_con, stmt, err)) return false;
    }
  }
  g_sealed = true;
  std::cerr << "sidecar: sealed " << g_tables.size() << " table(s), "
            << (unlocked ? "unlocked" : "read-only and locked") << std::endl;
  return true;
}

void Worker() {
  for (;;) {
    std::string id, sql;
    {
      std::unique_lock<std::mutex> lock(g_work_mu);
      g_work_cv.wait(lock, [] { return g_work_ready || g_stop; });
      if (g_stop) return;
      id = g_work_id;
      sql = g_work_sql;
      g_work_ready = false;
    }
    const auto start = std::chrono::steady_clock::now();
    try {
      auto res = g_con->Query(sql);
      const auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(
                          std::chrono::steady_clock::now() - start)
                          .count();
      if (res->HasError()) {
        ErrorReply(id, Classify(res->GetError()), res->GetError());
      } else {
        ResultReply(id, *res, ms);
      }
    } catch (const std::exception &e) {
      ErrorReply(id, "internal_error", e.what());
    }
    g_busy = false;
  }
}

int Run() {
  const char *data_dir_env = std::getenv("SIDECAR_DATA_DIR");
  if (data_dir_env == nullptr || *data_dir_env == '\0') {
    std::cerr << "sidecar: SIDECAR_DATA_DIR is required" << std::endl;
    return 2;
  }
  const std::string data_dir = data_dir_env;
  const char *ext_env = std::getenv("SIDECAR_EXT_DIR");
  const std::string ext_dir = ext_env == nullptr ? "" : ext_env;
  const char *unlocked_env = std::getenv("SIDECAR_UNLOCKED");
  const bool unlocked = unlocked_env != nullptr && *unlocked_env == '1';
  std::cerr << "sidecar: starting, data_dir=" << data_dir << " unlocked=" << unlocked << std::endl;

  std::thread worker(Worker);
  std::string busy_id;
  std::string line;
  while (std::getline(std::cin, line)) {
    const auto f = Split(line, '\t');
    if (f.size() < 2) continue;
    const std::string id = f[0];
    const std::string op = f[1];

    if (op == "shutdown") {
      if (g_busy && g_con) g_con->Interrupt();
      Reply(id, "");
      break;
    }
    if (op == "cancel") {
      if (g_busy && g_con) g_con->Interrupt();
      if (id != "0") Reply(id, "");
      continue;
    }
    if (op == "memory_limit") {
      Reply(id, "");
      continue;
    }
    if (op == "init") {
      if (f.size() < 6) {
        ErrorReply(id, "internal_error", "init needs dataDir, snapshot, table and location");
        continue;
      }
      if (g_sealed) {
        ErrorReply(id, "internal_error", "init after the first query; the sidecar is sealed");
        continue;
      }
      const std::string dir = f[2] + "/" + f[5] + "/metadata";
      const std::string meta = LatestMetadata(dir);
      if (meta.empty()) {
        ErrorReply(id, "not_found", "no .metadata.json under " + dir);
        continue;
      }
      const long long snapshot = atoll(f[3].c_str());
      std::string snap_err;
      if (!HasSnapshot(ext_dir, meta, snapshot, snap_err)) {
        ErrorReply(id, "not_found", snap_err);
        continue;
      }
      g_tables.push_back(TableSpec{f[4], meta, snapshot});
      Reply(id, "registered " + f[4] + " at " + f[3]);
      continue;
    }
    if (op == "query") {
      if (f.size() < 3) {
        ErrorReply(id, "internal_error", "query needs sql");
        continue;
      }
      if (g_tables.empty()) {
        ErrorReply(id, "internal_error", "no table registered; send init first");
        continue;
      }
      if (!g_sealed) {
        std::string err;
        bool ok = false;
        try {
          ok = Seal(data_dir, ext_dir, unlocked, err);
        } catch (const std::exception &e) {
          err = e.what();
        }
        if (!ok) {
          std::cerr << "sidecar: sealing failed, refusing to run: " << err << std::endl;
          ErrorReply(id, Classify(err), err);
          g_stop = true;
          g_work_cv.notify_all();
          worker.join();
          return 3;
        }
      }
      if (g_busy.exchange(true)) {
        ErrorReply(id, "internal_error", "a query is already running");
        continue;
      }
      {
        std::lock_guard<std::mutex> lock(g_work_mu);
        g_work_id = id;
        g_work_sql = f[2];
        g_work_ready = true;
      }
      g_work_cv.notify_one();
      continue;
    }
    ErrorReply(id, "internal_error", "unknown op " + op);
  }
  {
    std::lock_guard<std::mutex> lock(g_work_mu);
    g_stop = true;
  }
  g_work_cv.notify_all();
  worker.join();
  g_con.reset();
  g_db.reset();
  if (!g_catalog_path.empty()) {
    std::error_code ec;
    std::filesystem::remove(g_catalog_path, ec);
    std::filesystem::remove(g_catalog_path + ".wal", ec);
  }
  return 0;
}

}  // namespace

int main() {
  try {
    return Run();
  } catch (const std::exception &e) {
    std::cerr << "sidecar: uncaught exception: " << e.what() << std::endl;
    return 4;
  }
}
