// Sidecar for DuckDB: reads the spike protocol on stdin, registers one
// namespace's Iceberg table at a pinned snapshot, and runs caller SQL.
//
// Built against prebuilt libduckdb with the iceberg and httpfs extensions
// pre-placed in the extension directory, so nothing downloads at runtime. The
// confinement settings are applied through DBConfig before the database starts,
// because enable_external_access cannot be set once the process is up. See
// ../driver/PROTOCOL.md for the wire.
#include "duckdb.hpp"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <iostream>
#include <sstream>
#include <string>
#include <vector>

namespace {

std::string g_table_name;
std::string g_table_location;
long long g_snapshot = -1;
bool g_registered = false;
duckdb::DuckDB *g_db = nullptr;

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

void Reply(const std::string &id, const std::string &payload) {
  std::cout << id << "\tok\t" << payload << std::endl;
}

void ErrorReply(const std::string &id, const std::string &cls, const std::string &msg) {
  std::cout << id << "\terror\t" << cls << "\t" << msg << std::endl;
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

// The newest .metadata.json under dir. Iceberg writes one per commit and names
// them by version, so newest-by-write-time is the current table state; the
// snapshot id from init is what actually decides which data is read.
std::string LatestMetadata(const std::string &dir) {
  namespace fs = std::filesystem;
  std::error_code ec;
  std::string best;
  std::filesystem::file_time_type best_time{};
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

std::string Classify(const std::string &msg) {
  std::string lower = msg;
  for (auto &c : lower) c = static_cast<char>(tolower(c));
  if (lower.find("does not exist") != std::string::npos ||
      lower.find("no files found") != std::string::npos ||
      lower.find("catalog error") != std::string::npos) {
    return "not_found";
  }
  if (lower.find("not implemented") != std::string::npos ||
      lower.find("disabled") != std::string::npos ||
      lower.find("not supported") != std::string::npos ||
      lower.find("parser error") != std::string::npos ||
      lower.find("no function matches") != std::string::npos ||
      lower.find("binder error") != std::string::npos ||
      lower.find("cannot be loaded") != std::string::npos ||
      lower.find("not allowed") != std::string::npos) {
    return "not_supported";
  }
  return "query_error";
}

// Render a result as columns;rows;elapsed;truncated, the shape PROTOCOL.md
// specifies. Values are escaped so a value can never split the framing.
void ResultReply(const std::string &id, duckdb::MaterializedQueryResult &res, long long elapsed) {
  std::ostringstream cols;
  const auto &types = res.Collection().Types();
  for (duckdb::idx_t i = 0; i < res.ColumnCount(); i++) {
    if (i) cols << ";";
    cols << res.ColumnName(i) << ":" << types[i].ToString();
  }
  std::ostringstream rows;
  bool first = true;
  for (duckdb::idx_t i = 0; i < res.RowCount(); i++) {
    if (!first) rows << "|";
    first = false;
    for (duckdb::idx_t c = 0; c < res.ColumnCount(); c++) {
      if (c) rows << "\x1f";
      rows << Escape(res.GetValue(c, i).ToString());
    }
  }
  std::cout << id << "\tok\t" << cols.str() << "\t" << rows.str() << "\t" << elapsed << "\t0"
            << std::endl;
}

// Confinement is applied through DBConfig, not through a .duckdbrc.
//
// The first CI run wrote the settings to $HOME/.duckdbrc and every escape in
// the battery succeeded. The reason is that .duckdbrc is a *CLI* feature: the
// duckdb shell reads it, and an embedded DuckDB opened as DuckDB(nullptr)
// never looks at it. Nothing warns, so the process looks configured and is not.
//
// The ordering is measured, not guessed, and it is the reason the settings are
// split across two mechanisms:
//
//   1. allowed_directories goes in DBConfigOptions::allowed_directories, a
//      struct field, because it cannot be set once enable_external_access is
//      false (#533 measured that).
//   2. The rest go in unrecognized_options, which DuckDB applies at startup
//      before the database is usable. enable_external_access = false is the
//      guard and goes here rather than in a struct field.
//   3. lock_configuration = true goes last, by SET on a live connection, so
//      nothing above can be reopened afterwards.
void Configure(duckdb::DBConfig &cfg, const std::string &data_dir, const std::string &ext_dir) {
  cfg.options.allowed_directories.insert(data_dir);
  if (!ext_dir.empty()) {
    cfg.options.unrecognized_options["extension_directory"] = duckdb::Value(ext_dir);
  }
  cfg.options.unrecognized_options["enable_external_access"] = duckdb::Value::BOOLEAN(false);
  cfg.options.unrecognized_options["autoinstall_known_extensions"] = duckdb::Value::BOOLEAN(false);
  cfg.options.unrecognized_options["autoload_known_extensions"] = duckdb::Value::BOOLEAN(false);
  cfg.options.unrecognized_options["allow_persistent_secrets"] = duckdb::Value::BOOLEAN(false);
}

bool RegisterTable(duckdb::Connection &con, std::string &err) {
  if (g_registered) return true;
  if (g_table_name.empty() || g_snapshot < 0) return true;
  const std::string dir = g_table_location + "/metadata";
  const std::string meta = LatestMetadata(dir);
  if (meta.empty()) {
    err = "no .metadata.json under " + dir;
    return false;
  }
  // snapshot_from_id is the numeric option; `version` names a tag and is
  // silently ignored when given an id, which looks like a pinned read that is
  // not pinned.
  std::string sql = "LOAD iceberg; CREATE OR REPLACE VIEW " + g_table_name +
                    " AS SELECT * FROM iceberg_scan('" + meta + "', snapshot_from_id => " +
                    std::to_string(g_snapshot) + ");";
  auto res = con.Query(sql);
  if (res->HasError()) {
    err = res->GetError();
    return false;
  }
  g_registered = true;
  return true;
}

int Run() {
  const char *data_dir_env = std::getenv("SIDECAR_DATA_DIR");
  const char *ext_env = std::getenv("SIDECAR_EXT_DIR");
  const char *mem_env = std::getenv("SIDECAR_MEMORY_MAX");

  if (data_dir_env == nullptr || *data_dir_env == '\0') {
    std::cerr << "sidecar: SIDECAR_DATA_DIR is required" << std::endl;
    return 2;
  }
  const std::string data_dir = data_dir_env;
  const std::string ext_dir = ext_env == nullptr ? "" : ext_env;

  const char *unlocked_env = std::getenv("SIDECAR_UNLOCKED");
  const bool unlocked = unlocked_env != nullptr && *unlocked_env == '1';

  duckdb::DBConfig cfg;
  // stdout is the response channel of the spike protocol, and DuckDB writes its
  // own logging there ("Loading extension iceberg from ..."), which desynchronises
  // the framing and misattributes one request's answer to another. Engine logging
  // is therefore off, and the driver additionally skips any line that does not
  // carry the request id rather than failing on it.
  cfg.options.log_config.enabled = false;
  if (!unlocked) {
    Configure(cfg, data_dir, ext_dir);
  }
  if (mem_env != nullptr && *mem_env != '\0') {
    // maximum_memory is DuckDB's byte-valued knob. It has to be a struct field
    // too, because lock_configuration is on before a SET could reach it.
    const char *digits = mem_env;
    idx_t bytes = 0;
    for (const char *p = mem_env; *p != '\0'; p++) {
      if (*p >= '0' && *p <= '9') {
        bytes = bytes * 10 + static_cast<idx_t>(*p - '0');
      } else if (*p == 'K' || *p == 'k') {
        bytes *= 1024ULL;
      } else if (*p == 'M' || *p == 'm') {
        bytes *= 1024ULL * 1024ULL;
      } else if (*p == 'G' || *p == 'g') {
        bytes *= 1024ULL * 1024ULL * 1024ULL;
      }
    }
    (void)digits;
    cfg.options.maximum_memory = bytes;
  }
  g_db = new duckdb::DuckDB(nullptr, &cfg);
  if (!unlocked) {
    duckdb::Connection lock_con(*g_db);
    auto locked_res = lock_con.Query("SET lock_configuration = true");
    if (locked_res->HasError()) {
      std::cerr << "sidecar: lock_configuration was refused: " << locked_res->GetError() << std::endl;
      return 3;
    }
  }

  std::string line;
  while (std::getline(std::cin, line)) {
    const auto f = Split(line, '\t');
    if (f.size() < 2) continue;
    const std::string id = f[0];
    const std::string op = f[1];

    if (op == "shutdown") {
      Reply(id, "");
      break;
    }
    if (op == "cancel") {
      duckdb::Connection con(*g_db);
      con.Interrupt();
      Reply(id, "");
      continue;
    }
    if (op == "memory_limit") {
      Reply(id, "");
      continue;
    }
    if (op == "init") {
      // init <dataDir> <snapshot> <table> <location>
      if (f.size() < 6) {
        ErrorReply(id, "internal_error", "init needs dataDir, snapshot, table and location");
        continue;
      }
      g_snapshot = atoll(f[3].c_str());
      g_table_name = f[4];
      g_table_location = std::string(f[2]) + "/" + f[5];
      duckdb::Connection con(*g_db);
      std::string err;
      if (!RegisterTable(con, err)) {
        ErrorReply(id, Classify(err), err);
        continue;
      }
      Reply(id, "");
      continue;
    }
    if (op == "query") {
      if (f.size() < 3) {
        ErrorReply(id, "internal_error", "query needs sql");
        continue;
      }
      duckdb::Connection con(*g_db);
      const auto start = std::chrono::steady_clock::now();
      auto res = con.Query(f[2]);
      const auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(
                          std::chrono::steady_clock::now() - start)
                          .count();
      if (res->HasError()) {
        ErrorReply(id, Classify(res->GetError()), res->GetError());
        continue;
      }
      ResultReply(id, *res, ms);
      continue;
    }
    ErrorReply(id, "internal_error", "unknown op " + op);
  }
  delete g_db;
  return 0;
}

}  // namespace

int main() { return Run(); }
