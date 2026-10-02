// Sidecar for DuckDB: reads the spike protocol on stdin, registers one
// namespace's Iceberg table at a pinned snapshot, and runs caller SQL.
//
// Built against prebuilt libduckdb with the iceberg and httpfs extensions
// pre-placed in the extension directory, so nothing downloads at runtime. The
// confinement settings are written into a per-process .duckdbrc before the
// database starts, because enable_external_access cannot be set once the
// process is up. See ../driver/PROTOCOL.md for the wire.
#include "duckdb.hpp"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>
#include <vector>

namespace {

std::string g_table_name;
std::string g_table_location;
long long g_snapshot = -1;
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

// The order is measured, not guessed: allowed_directories cannot be set once
// enable_external_access is false, and nothing here can be changed once
// lock_configuration is on. This is the lock #533 measured.
void WriteDuckDBRC(const std::string &home, const std::string &data_dir, const std::string &ext_dir) {
  const std::string path = home + "/.duckdbrc";
  std::ofstream f(path);
  f << "SET allowed_directories = ['" << data_dir << "'];\n";
  f << "SET extension_directory = '" << ext_dir << "';\n";
  f << "SET enable_external_access = false;\n";
  f << "SET autoinstall_known_extensions = false;\n";
  f << "SET autoload_known_extensions = false;\n";
  f << "SET allow_persistent_secrets = false;\n";
  f << "SET lock_configuration = true;\n";
  f.close();
}

bool RegisterTable(duckdb::Connection &con, std::string &err) {
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
  return true;
}

int Run() {
  const char *data_dir_env = std::getenv("SIDECAR_DATA_DIR");
  const char *ext_env = std::getenv("SIDECAR_EXT_DIR");
  const char *home_env = std::getenv("SIDECAR_HOME");
  const char *mem_env = std::getenv("SIDECAR_MEMORY_MAX");

  if (data_dir_env == nullptr || *data_dir_env == '\0') {
    std::cerr << "sidecar: SIDECAR_DATA_DIR is required" << std::endl;
    return 2;
  }
  const std::string data_dir = data_dir_env;
  const std::string ext_dir = ext_env == nullptr ? "" : ext_env;
  const std::string home = home_env == nullptr ? "/tmp" : home_env;

  const char *unlocked_env = std::getenv("SIDECAR_UNLOCKED");
  const bool unlocked = unlocked_env != nullptr && *unlocked_env == '1';
  std::filesystem::create_directories(home);
  if (!unlocked) {
    WriteDuckDBRC(home, data_dir, ext_dir);
  }
  setenv("HOME", home.c_str(), 1);

  if (mem_env != nullptr && *mem_env != '\0') {
    duckdb::DBConfig cfg;
    // maximum_memory is DuckDB's byte-valued knob; the .duckdbrc cannot set it
    // because lock_configuration is already on by the time a SET would run.
    cfg.options.maximum_memory = duckdb::idx_t(atoll(mem_env));
    g_db = new duckdb::DuckDB(nullptr, &cfg);
  } else {
    g_db = new duckdb::DuckDB(nullptr);
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
