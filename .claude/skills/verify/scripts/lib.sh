# Sourced by serve.sh and mcp-stdio.sh. Drops the caller's dolmen settings,
# as internal/blackbox's hermeticEnv does, keeping only DOLMEN_ADMIN_KEY so
# `serve.sh -auth on` can bootstrap.
scrub_env() {
  local v
  for v in $(compgen -e); do
    case "$v" in
      DOLMEN_ADMIN_KEY) ;;
      DOLMEN_*|OPENAI_API_KEY|REMBED_CACHE|HF_TOKEN) unset "$v" ;;
    esac
  done
}
