// dolmen-duckdb: the lakehouse engine's SQL sidecar. One process serves one
// namespace. The wire is described in PROTOCOL.md next to this file.
//
// Lifecycle:
//   1. define ops run trusted statements, built by dolmen, against a private
//      writable catalog file: the extension load and one view per table.
//   2. seal reopens that file READ_ONLY with the confinement applied (see
//      Seal). After seal, define is refused and only caller queries run.
//   3. Queries run on a worker thread so the reader thread can deliver a
//      cancel to Connection::Interrupt while a statement is running.
#include "duckdb.hpp"

#include <atomic>
#include <chrono>
#include <cmath>
#include <condition_variable>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <iostream>
#include <memory>
#include <mutex>
#include <random>
#include <string>
#include <thread>
#include <vector>

namespace {

std::mutex g_out_mu;
std::unique_ptr<duckdb::DuckDB> g_seed_db;
std::unique_ptr<duckdb::Connection> g_seed_con;
std::unique_ptr<duckdb::DuckDB> g_db;
std::unique_ptr<duckdb::Connection> g_con;
std::string g_catalog_path;
bool g_sealed = false;

struct Work {
  std::string id;
  long long offset = 0;
  long long limit = 0;
  long long max_bytes = 0;
  std::string sql;
  std::vector<std::string> args;
};

std::mutex g_work_mu;
std::condition_variable g_work_cv;
Work g_work;
bool g_work_ready = false;
std::atomic<bool> g_busy{false};
bool g_stop = false;

std::vector<std::string> Split(const std::string &line) {
  std::vector<std::string> out;
  std::string cur;
  for (size_t i = 0; i < line.size(); i++) {
    const char c = line[i];
    if (c == '\\' && i + 1 < line.size()) {
      const char n = line[++i];
      if (n == 't') cur.push_back('\t');
      else if (n == 'n') cur.push_back('\n');
      else if (n == 'r') cur.push_back('\r');
      else cur.push_back(n);
    } else if (c == '\t') {
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
  out.reserve(v.size());
  for (char c : v) {
    if (c == '\\') out += "\\\\";
    else if (c == '\t') out += "\\t";
    else if (c == '\n') out += "\\n";
    else if (c == '\r') out += "\\r";
    else out.push_back(c);
  }
  return out;
}

void Emit(const std::string &line) {
  std::lock_guard<std::mutex> lock(g_out_mu);
  std::cout << line << '\n' << std::flush;
}

void Reply(const std::string &id) { Emit(id + "\tok"); }

std::string ErrorLine(const std::string &id, const std::string &cls, const std::string &msg) {
  return id + "\terror\t" + cls + "\t" + Escape(msg);
}

void ErrorReply(const std::string &id, const std::string &cls, const std::string &msg) {
  Emit(ErrorLine(id, cls, msg));
}

std::string Classify(const std::string &msg) {
  std::string lower = msg;
  for (auto &c : lower) c = static_cast<char>(tolower(static_cast<unsigned char>(c)));
  if (lower.find("interrupted") != std::string::npos) return "canceled";
  if (lower.find("multiple statements") != std::string::npos) return "invalid";
  if (lower.find("out of memory") != std::string::npos) return "resource";
  return "query_error";
}

bool Exec(duckdb::Connection &con, const std::string &sql, std::string &err) {
  auto res = con.Query(sql);
  if (res->HasError()) {
    err = res->GetError();
    return false;
  }
  return true;
}

std::string Quote(const std::string &v) {
  std::string out = "'";
  for (char c : v) {
    if (c == '\'') out += "''";
    else out.push_back(c);
  }
  return out + "'";
}

duckdb::idx_t ParseBytes(const char *v) {
  duckdb::idx_t bytes = 0;
  for (const char *p = v; *p != '\0'; p++) {
    if (*p >= '0' && *p <= '9') bytes = bytes * 10 + static_cast<duckdb::idx_t>(*p - '0');
  }
  return bytes;
}

std::string Getenv(const char *name) {
  const char *v = std::getenv(name);
  return v == nullptr ? "" : v;
}

// Turned off before any LOAD, so a missing pinned extension fails instead of
// being downloaded.
bool NoAutoinstall(duckdb::Connection &con, std::string &err) {
  return Exec(con, "SET autoinstall_known_extensions = false", err) &&
         Exec(con, "SET autoload_known_extensions = false", err);
}

bool OpenSeed(const std::string &ext_dir, std::string &err) {
  namespace fs = std::filesystem;
  std::mt19937_64 rng(std::random_device{}());
  g_catalog_path = (fs::temp_directory_path() /
                    ("dolmen-duckdb-" + std::to_string(rng()) + ".duckdb"))
                       .string();
  duckdb::DBConfig cfg;
  cfg.options.log_config.enabled = false;
  g_seed_db = std::make_unique<duckdb::DuckDB>(g_catalog_path.c_str(), &cfg);
  g_seed_con = std::make_unique<duckdb::Connection>(*g_seed_db);
  if (!ext_dir.empty() && !Exec(*g_seed_con, "SET extension_directory = " + Quote(ext_dir), err)) return false;
  if (!NoAutoinstall(*g_seed_con, err)) return false;
  return Exec(*g_seed_con, "LOAD avro", err) && Exec(*g_seed_con, "LOAD iceberg", err);
}

// The confinement, in the order that makes it hold: the views are written to a
// private file and the file is closed; it is reopened READ_ONLY with the data
// directory as the only allowed one and the memory ceiling fixed in the config;
// iceberg is loaded while loading is still possible; then external access,
// extension loading and secrets are switched off and the configuration locked.
bool Seal(const std::string &data_dir, const std::string &ext_dir, std::string &err) {
  namespace fs = std::filesystem;
  if (!Exec(*g_seed_con, "CHECKPOINT", err)) return false;
  g_seed_con.reset();
  g_seed_db.reset();

  duckdb::DBConfig cfg;
  cfg.options.log_config.enabled = false;
  const std::string mem = Getenv("DOLMEN_DUCKDB_MEMORY");
  if (!mem.empty() && mem != "0") cfg.options.maximum_memory = ParseBytes(mem.c_str());
  const std::string threads = Getenv("DOLMEN_DUCKDB_THREADS");
  if (!threads.empty() && threads != "0") cfg.options.maximum_threads = ParseBytes(threads.c_str());
  const std::string spill_dir = g_catalog_path + ".tmp";
  std::error_code ec;
  fs::create_directories(spill_dir, ec);
  cfg.options.temporary_directory = spill_dir;
  cfg.options.access_mode = duckdb::AccessMode::READ_ONLY;
  cfg.options.allowed_directories.insert(data_dir);
  // Iceberg metadata names files by URI, and DuckDB matches the allowed list by
  // prefix, so the same directory is allowed in its file:// spellings too.
  cfg.options.allowed_directories.insert("file://" + data_dir);
  if (data_dir.size() > 1 && data_dir[1] == ':') {
    cfg.options.allowed_directories.insert("file:///" + data_dir);
    cfg.options.allowed_directories.insert("/" + data_dir);
  }
  cfg.options.allowed_directories.insert(spill_dir);
  g_db = std::make_unique<duckdb::DuckDB>(g_catalog_path.c_str(), &cfg);
  g_con = std::make_unique<duckdb::Connection>(*g_db);
  if (!ext_dir.empty() && !Exec(*g_con, "SET extension_directory = " + Quote(ext_dir), err)) return false;
  if (!NoAutoinstall(*g_con, err)) return false;
  if (!Exec(*g_con, "LOAD avro", err) || !Exec(*g_con, "LOAD iceberg", err)) return false;
  for (const char *stmt : {"SET enable_external_access = false", "SET autoinstall_known_extensions = false",
                           "SET autoload_known_extensions = false", "SET allow_persistent_secrets = false",
                           "SET lock_configuration = true"}) {
    if (!Exec(*g_con, stmt, err)) return false;
  }
  g_sealed = true;
  return true;
}

duckdb::Value Arg(const std::string &raw) {
  if (raw.empty() || raw[0] == 'n') return duckdb::Value();
  const std::string body = raw.substr(1);
  switch (raw[0]) {
  case 'i':
    return duckdb::Value::BIGINT(std::stoll(body));
  case 'f':
    return duckdb::Value::DOUBLE(std::stod(body));
  case 'b':
    return duckdb::Value::BOOLEAN(body == "1");
  default:
    return duckdb::Value(body);
  }
}

std::string Hex(const std::string &bytes) {
  static const char *digits = "0123456789abcdef";
  std::string out;
  out.reserve(bytes.size() * 2);
  for (unsigned char c : bytes) {
    out.push_back(digits[c >> 4]);
    out.push_back(digits[c & 15]);
  }
  return out;
}

std::string Cell(const duckdb::Value &v) {
  if (v.IsNull()) return "n";
  switch (v.type().id()) {
  case duckdb::LogicalTypeId::BOOLEAN:
    return v.GetValue<bool>() ? "b1" : "b0";
  case duckdb::LogicalTypeId::TINYINT:
  case duckdb::LogicalTypeId::SMALLINT:
  case duckdb::LogicalTypeId::INTEGER:
  case duckdb::LogicalTypeId::BIGINT:
  case duckdb::LogicalTypeId::UTINYINT:
  case duckdb::LogicalTypeId::USMALLINT:
  case duckdb::LogicalTypeId::UINTEGER:
    return "i" + std::to_string(v.GetValue<int64_t>());
  case duckdb::LogicalTypeId::UBIGINT:
  case duckdb::LogicalTypeId::HUGEINT:
  case duckdb::LogicalTypeId::UHUGEINT:
  case duckdb::LogicalTypeId::DECIMAL:
    return "d" + Escape(v.ToString());
  case duckdb::LogicalTypeId::FLOAT:
  case duckdb::LogicalTypeId::DOUBLE: {
    const double d = v.GetValue<double>();
    if (!std::isfinite(d)) return "s" + Escape(v.ToString());
    char buf[40];
    std::snprintf(buf, sizeof(buf), "%.17g", d);
    return std::string("f") + buf;
  }
  case duckdb::LogicalTypeId::BLOB:
    return "x" + Hex(duckdb::StringValue::Get(v));
  case duckdb::LogicalTypeId::VARCHAR:
    return "s" + Escape(duckdb::StringValue::Get(v));
  default:
    return "s" + Escape(v.ToString());
  }
}

std::string Run(const Work &w) {
  auto prepared = g_con->Prepare(w.sql);
  if (prepared->HasError()) {
    return ErrorLine(w.id, Classify(prepared->GetError()), prepared->GetError());
  }
  duckdb::vector<duckdb::Value> values;
  for (const auto &a : w.args) values.push_back(Arg(a));
  auto res = prepared->Execute(values, true);
  if (res->HasError()) {
    return ErrorLine(w.id, Classify(res->GetError()), res->GetError());
  }
  std::string head = "\t" + std::to_string(res->ColumnCount());
  for (duckdb::idx_t c = 0; c < res->ColumnCount(); c++) {
    head += "\t" + Escape(res->ColumnName(c)) + "\t" + Escape(res->types[c].ToString());
  }
  std::string body;
  long long skipped = 0;
  long long rows = 0;
  bool truncated = false;
  for (;;) {
    auto chunk = res->Fetch();
    if (res->HasError()) {
      return ErrorLine(w.id, Classify(res->GetError()), res->GetError());
    }
    if (!chunk || chunk->size() == 0) break;
    for (duckdb::idx_t r = 0; r < chunk->size() && !truncated; r++) {
      if (skipped < w.offset) {
        skipped++;
        continue;
      }
      if (rows == w.limit) {
        truncated = true;
        break;
      }
      std::string line;
      for (duckdb::idx_t c = 0; c < chunk->ColumnCount(); c++) line += "\t" + Cell(chunk->GetValue(c, r));
      if (w.max_bytes > 0 && static_cast<long long>(body.size() + line.size()) > w.max_bytes) {
        if (rows == 0) {
          return ErrorLine(w.id, "too_large", "the first row alone exceeds the response budget");
        }
        truncated = true;
        break;
      }
      body += line;
      rows++;
    }
    if (truncated) break;
  }
  return w.id + "\tok\t" + (truncated ? "1" : "0") + head + "\t" + std::to_string(rows) + body;
}

void Worker() {
  for (;;) {
    Work w;
    {
      std::unique_lock<std::mutex> lock(g_work_mu);
      g_work_cv.wait(lock, [] { return g_work_ready || g_stop; });
      if (g_stop) return;
      w = g_work;
      g_work_ready = false;
    }
    std::string reply;
    try {
      reply = Run(w);
    } catch (const std::exception &e) {
      reply = ErrorLine(w.id, Classify(e.what()), e.what());
    }
    g_busy = false;
    Emit(reply);
  }
}

int Main() {
  const std::string data_dir = Getenv("DOLMEN_DUCKDB_DATA_DIR");
  if (data_dir.empty()) {
    std::cerr << "dolmen-duckdb: DOLMEN_DUCKDB_DATA_DIR is required" << std::endl;
    return 2;
  }
  const std::string ext_dir = Getenv("DOLMEN_DUCKDB_EXTENSION_DIR");
  std::string err;
  if (!OpenSeed(ext_dir, err)) {
    std::cerr << "dolmen-duckdb: cannot start: " << err << std::endl;
    return 3;
  }
  Emit("0\tready");
  std::thread worker(Worker);
  std::string line;
  while (std::getline(std::cin, line)) {
    auto f = Split(line);
    if (f.size() < 2) continue;
    const std::string &id = f[0];
    const std::string &op = f[1];
    if (op == "shutdown") {
      if (g_busy && g_con) g_con->Interrupt();
      Reply(id);
      break;
    }
    if (op == "cancel") {
      if (g_busy && g_con) g_con->Interrupt();
      continue;
    }
    if (op == "define") {
      if (g_sealed || f.size() < 3) {
        ErrorReply(id, "internal", g_sealed ? "define after seal" : "define needs a statement");
        continue;
      }
      if (!Exec(*g_seed_con, f[2], err)) {
        ErrorReply(id, "internal", err);
        continue;
      }
      Reply(id);
      continue;
    }
    if (op == "seal") {
      if (g_sealed) {
        Reply(id);
        continue;
      }
      bool ok = false;
      try {
        ok = Seal(data_dir, ext_dir, err);
      } catch (const std::exception &e) {
        err = e.what();
      }
      if (!ok) {
        ErrorReply(id, "internal", "sealing failed, refusing to run: " + err);
        break;
      }
      Reply(id);
      continue;
    }
    if (op == "query") {
      if (!g_sealed) {
        ErrorReply(id, "internal", "query before seal");
        continue;
      }
      if (f.size() < 6) {
        ErrorReply(id, "internal", "query needs offset, limit, max_bytes and sql");
        continue;
      }
      if (g_busy.exchange(true)) {
        ErrorReply(id, "internal", "a query is already running");
        continue;
      }
      {
        std::lock_guard<std::mutex> lock(g_work_mu);
        g_work.id = id;
        g_work.offset = std::stoll(f[2]);
        g_work.limit = std::stoll(f[3]);
        g_work.max_bytes = std::stoll(f[4]);
        g_work.sql = f[5];
        g_work.args.assign(f.begin() + 6, f.end());
        g_work_ready = true;
      }
      g_work_cv.notify_one();
      continue;
    }
    ErrorReply(id, "internal", "unknown op " + op);
  }
  {
    std::lock_guard<std::mutex> lock(g_work_mu);
    g_stop = true;
  }
  g_work_cv.notify_all();
  worker.join();
  g_con.reset();
  g_db.reset();
  g_seed_con.reset();
  g_seed_db.reset();
  std::error_code ec;
  std::filesystem::remove(g_catalog_path, ec);
  std::filesystem::remove(g_catalog_path + ".wal", ec);
  std::filesystem::remove_all(g_catalog_path + ".tmp", ec);
  return 0;
}

}  // namespace

int main() {
  std::ios::sync_with_stdio(false);
  try {
    return Main();
  } catch (const std::exception &e) {
    std::cerr << "dolmen-duckdb: uncaught exception: " << e.what() << std::endl;
    return 4;
  }
}
