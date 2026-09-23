package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/postgres"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

type envHelp struct {
	key  string
	desc string
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var pe *printedError
		if errors.As(err, &pe) {
			os.Exit(1)
		}
		slog.Error("dolmen exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "mcp":
			return runStdio(args[1:])
		case "backup":
			return runBackup(args[1:], os.Getenv, os.Stdout, os.Stderr)
		case "restore":
			return runRestore(args[1:], os.Getenv, os.Stdout, os.Stderr)
		}
	}
	cfg, err := loadConfig(args, os.Getenv, os.LookupEnv, os.Stderr, false)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if cfg.Version {
		fmt.Println("dolmen", version.Version)
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	emb, err := newEmbedProvider(cfg)
	if err != nil {
		return err
	}

	grants, err := openGrantRegistry(cfg)
	if err != nil {
		return err
	}
	defer grants.Close()

	oidcSrc, err := buildOIDC(cfg, grants)
	if err != nil {
		return err
	}

	apiSrv := api.New(st, emb, api.WithBaseURL(cfg.BaseURL), api.WithNamespaceHint(cfg.SkillNamespaceHint), api.WithPrefix(cfg.Prefix), api.WithMaxSubscriptionAge(cfg.MaxSubscriptionAge), api.WithAuth(cfg.Auth), api.WithGrants(grants), api.WithOIDC(oidcSrc), api.WithTimeouts(cfg.Timeouts))
	mcpSrv := newMCPServer(cfg, apiSrv)

	sub := http.NewServeMux()
	sub.Handle("/mcp", mcpSrv)
	sub.Handle("/", apiSrv.Handler())
	router := withPrefix(cfg.Prefix, sub)

	httpSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: api.OriginGuard(router, cfg.AllowedOrigins),
	}
	cfg.Timeouts.Configure(httpSrv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("dolmen listening", "addr", cfg.Addr, "data", cfg.DataDir, "embed", emb.Name(), "version", version.Version)
		slog.Info("endpoints", "mcp", "http://"+cfg.Addr+cfg.Prefix+"/mcp", "api", "http://"+cfg.Addr+cfg.Prefix+"/v1/{op}", "health", "http://"+cfg.Addr+cfg.Prefix+"/healthz", "version", "http://"+cfg.Addr+cfg.Prefix+"/version", "skills", "http://"+cfg.Addr+cfg.Prefix+"/skills")
		logAuthPosture(cfg.Auth)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func runStdio(args []string) error {
	cfg, err := loadConfig(args, os.Getenv, os.LookupEnv, os.Stderr, true)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if cfg.Version {
		fmt.Println("dolmen", version.Version)
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	emb, err := newEmbedProvider(cfg)
	if err != nil {
		return err
	}

	apiSrv := api.New(st, emb, api.WithBaseURL(cfg.BaseURL), api.WithNamespaceHint(cfg.SkillNamespaceHint), api.WithPrefix(cfg.Prefix), api.WithAuth(cfg.Auth), api.WithTimeouts(cfg.Timeouts))
	mcpSrv := newMCPServer(cfg, apiSrv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	slog.Info("dolmen mcp serving stdio", "data", cfg.DataDir, "embed", emb.Name(), "version", version.Version)
	return mcpSrv.ServeStdio(ctx, os.Stdin, os.Stdout)
}

func openStore(cfg *config) (store.Engine, error) {
	if cfg.Engine == store.EnginePostgres {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		retention := cfg.ChangeRetention
		st, err := postgres.Open(context.Background(), postgres.Config{
			DSN:             cfg.PostgresDSN,
			Catalog:         cfg.PostgresCatalog,
			QueryRole:       cfg.PostgresQueryRole,
			ChangeRetention: &retention,
			SharedFilter:    cfg.Auth.On(),
		})
		if err != nil {
			return nil, fmt.Errorf("open PostgreSQL catalog: %w", err)
		}
		return st, nil
	}
	st, err := store.Open(cfg.DataDir, store.WithChangeRetention(cfg.ChangeRetention), store.WithMaxOpenNamespaces(cfg.MaxOpenNamespaces))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return st, nil
}

func newEmbedProvider(cfg *config) (embed.Provider, error) {
	emb, err := embed.NewProvider(cfg.Embed.Provider, cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("embed provider: %w", err)
	}
	if l, ok := emb.(*embed.Local); ok {
		slog.Info("local embedding provider", "model", l.Model, "cache", "under the data directory (first use loads from the cache; downloads from the Hugging Face Hub only if the model is not pre-seeded)")
		if !l.Cached() {
			if l.HubModel() {
				slog.Warn("local embedding model is not cached; the first vectorized write will download it from the Hugging Face Hub", "model", l.Model)
			} else {
				slog.Warn("configured local model directory is incomplete; no download repairs it — fix or replace the directory (DOLMEN_EMBED_MODEL)", "model", l.Model)
			}
		}
	}
	return emb, nil
}

func openGrantRegistry(cfg *config) (*auth.Registry, error) {
	if !cfg.Auth.On() {
		return nil, nil
	}
	r, err := auth.OpenRegistry(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.Auth.UseKeys(r)
	if _, err := buildOIDC(cfg, r); err != nil {
		r.Close()
		return nil, err
	}
	if err := cfg.Auth.CheckRootAdministrator(context.Background(), r); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func buildOIDC(cfg *config, r *auth.Registry) (*auth.OIDCSource, error) {
	if r == nil || !cfg.OIDC.Enabled() {
		return nil, nil
	}
	if cfg.oidcSource != nil {
		return cfg.oidcSource, nil
	}
	ctx := context.Background()
	deployment, err := r.DeploymentID(ctx, cfg.OIDC.DeploymentID)
	if err != nil {
		return nil, err
	}
	ring, err := r.LoadKeyring(ctx, deployment)
	if err != nil {
		return nil, err
	}
	cfg.Auth.UseTokens(ring)
	cfg.Auth.RefreshTokensFrom(func(ctx context.Context) (auth.Keyring, error) {
		return r.LoadKeyring(ctx, deployment)
	}, 0)
	src := auth.NewOIDCSource(cfg.OIDC, r, ring, nil)
	src.PublishRingTo(cfg.Auth.UseTokens)
	cfg.Auth.SetOIDCIssuer(src.IssuerDigest())
	cfg.oidcSource = src
	return src, nil
}

func logAuthPosture(a *auth.Authenticator) {
	if a.On() {
		slog.Info("authentication on", "source", auth.AdminKeySourceName, "principal", auth.AdminPrincipal)
		return
	}
	slog.Warn("no authentication: keep this bound to a private interface")
}

func newMCPServer(cfg *config, apiSrv *api.Server) *mcp.Server {
	return mcp.New(apiSrv, cfg.AllowedOrigins, mcp.WithBaseURL(cfg.BaseURL), mcp.WithNamespaceHint(cfg.SkillNamespaceHint), mcp.WithPrefix(cfg.Prefix))
}

type printedError struct {
	err error
}

func (e *printedError) Error() string { return e.err.Error() }

type config struct {
	Addr               string
	DataDir            string
	Engine             string
	PostgresDSN        string
	PostgresCatalog    string
	PostgresQueryRole  string
	Auth               *auth.Authenticator
	OIDC               auth.OIDCConfig
	oidcSource         *auth.OIDCSource
	AllowedOrigins     []string
	Embed              embedConfig
	Version            bool
	BaseURL            string
	Prefix             string
	SkillNamespaceHint string
	ChangeRetention    time.Duration
	MaxSubscriptionAge time.Duration
	Timeouts           api.Timeouts
	MaxOpenNamespaces  int
}

type embedConfig struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
}

func loadConfig(args []string, getenv func(string) string, lookupEnv func(string) (string, bool), out io.Writer, stdio bool) (*config, error) {
	fs := flag.NewFlagSet("dolmen", flag.ContinueOnError)
	fs.SetOutput(out)

	addr := fs.String("addr", envOr("DOLMEN_ADDR", "127.0.0.1:8790", getenv), "listen address")
	dataDir := fs.String("data", envOr("DOLMEN_DATA", "data", getenv), "data directory (one SQLite file per namespace)")
	engine := fs.String("engine", getenv("DOLMEN_ENGINE"), "storage engine: sqlite (default) or postgres")
	pgDSN := fs.String("pg-dsn", getenv("DOLMEN_PG_DSN"), "PostgreSQL connection string; required when -engine postgres")
	pgCatalog := fs.String("pg-catalog", getenv("DOLMEN_PG_CATALOG"), "PostgreSQL catalog schema (default dolmen_catalog)")
	pgQueryRole := fs.String("pg-query-role", getenv("DOLMEN_PG_QUERY_ROLE"), "pre-provisioned restricted role that caller SQL runs as; required for the query op")
	authMode := fs.String("auth", envOr("DOLMEN_AUTH", "off", getenv), "authentication: off (default, no identity required) or on (deny-by-default; set DOLMEN_ADMIN_KEY on first start)")
	trustedProxies := fs.String("trusted-proxies", envOr("DOLMEN_TRUSTED_PROXIES", "", getenv), "comma-separated CIDRs (bare IPs allowed) whose peers may assert X-Dolmen-Principal / X-Dolmen-Groups")
	maxGroupsDefault, maxGroupsErr := envIntOr("DOLMEN_MAX_GROUPS", auth.DefaultMaxGroups, getenv)
	maxGroups := fs.Int("max-groups", maxGroupsDefault, "maximum group entries accepted per request (1 to 1024)")
	showVersion := fs.Bool("version", false, "print version and exit")
	publicBaseURL := fs.String("base-url", envOr("DOLMEN_BASE_URL", "", getenv), "public base URL for skills and MCP links (default: use request Host)")
	prefix := fs.String("prefix", envOr("DOLMEN_PREFIX", "", getenv), "mount all endpoints under this URL prefix (pass-through proxy)")

	maxOpenDefault, maxOpenErr := envIntOr("DOLMEN_MAX_OPEN_NAMESPACES", store.DefaultMaxOpenNamespaces, getenv)
	maxOpenNamespaces := fs.Int("max-open-namespaces", maxOpenDefault, "namespaces held open at once; past it, the least recently used idle namespace is closed until it is next used (at least 1; sqlite engine)")
	changeRetention := fs.String("change-retention", envOr("DOLMEN_CHANGE_RETENTION", "168h", getenv), "change-log retention: 0 disables pruning (records and cursors never expire); otherwise 1h to 2160h")
	maxSubscriptionAge := fs.String("max-subscription-age", envOr("DOLMEN_MAX_SUBSCRIPTION_AGE", "30m", getenv), "subscribe connection age bound: the stream teaching-closes at the bound and the client reconnects from its cursor; 0 disables the bound (the identity-refresh backstop is lost), otherwise 1s to 24h")
	readTimeout := fs.String("read-timeout", envOr("DOLMEN_READ_TIMEOUT", api.DefaultReadTimeout.String(), getenv), "time to read one request, headers and body: 0 disables the bound, otherwise 1s to 24h")
	writeTimeout := fs.String("write-timeout", envOr("DOLMEN_WRITE_TIMEOUT", api.DefaultWriteTimeout.String(), getenv), "time to write one response once it starts: 0 disables the bound, otherwise 1s to 24h")
	idleTimeout := fs.String("idle-timeout", envOr("DOLMEN_IDLE_TIMEOUT", api.DefaultIdleTimeout.String(), getenv), "time a keep-alive connection may wait for its next request: 0 disables the bound, otherwise 1s to 24h")
	maxHeaderBytesDefault, maxHeaderBytesErr := envIntOr("DOLMEN_MAX_HEADER_BYTES", api.DefaultMaxHeaderBytes, getenv)
	maxHeaderBytes := fs.Int("max-header-bytes", maxHeaderBytesDefault, fmt.Sprintf("largest request header block accepted, in bytes (%d to %d)", minHeaderBytes, maxHeaderBytesLimit))
	opTimeout := fs.String("op-timeout", envOr("DOLMEN_OP_TIMEOUT", api.DefaultOpTimeout.String(), getenv), "time for one operation's work, beyond wait_for's own timeout_ms; migrate has -migrate-timeout instead: 0 disables the bound, otherwise 1s to 24h")
	migrateTimeout := fs.String("migrate-timeout", envOr("DOLMEN_MIGRATE_TIMEOUT", "0", getenv), "time for one migrate call, which may backfill every row: 0 (default) leaves it unbounded, otherwise 1s to 24h")

	fs.Usage = func() {
		fmt.Fprint(out, "Usage: dolmen [flags]\n       dolmen mcp [flags]\n       dolmen backup -out DIR [-data DIR]\n       dolmen restore -from DIR [-data DIR]\n\nFlags:\n")
		fs.PrintDefaults()
		printEnvHelp(out)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, &printedError{err}
	}
	if fs.NArg() > 0 {
		e := fmt.Errorf("unexpected positional argument(s): %q", fs.Args())
		fmt.Fprintf(out, "%v\n", e)
		fs.Usage()
		return nil, &printedError{e}
	}
	if *showVersion {
		return &config{Version: true}, nil
	}

	allowedOrigins, err := parseAllowedOrigins(getenv("DOLMEN_ALLOWED_ORIGINS"))
	if err != nil {
		return nil, fmt.Errorf("DOLMEN_ALLOWED_ORIGINS: %w", err)
	}

	provider := envOr("DOLMEN_EMBED_PROVIDER", "local", getenv)
	baseURL := envOr("DOLMEN_EMBED_BASE_URL", "", getenv)
	model := envOr("DOLMEN_EMBED_MODEL", "", getenv)

	apiKey, ok := lookupEnv("DOLMEN_EMBED_API_KEY")
	if !ok {
		apiKey = getenv("OPENAI_API_KEY")
	}

	if _, err := embed.NewProvider(provider, baseURL, model, apiKey, *dataDir); err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	if err := store.ValidateEngine(*engine); err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}
	if *engine == store.EnginePostgres && *pgDSN == "" {
		err := fmt.Errorf("engine %q needs a connection; pass -pg-dsn or set DOLMEN_PG_DSN", store.EnginePostgres)
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}
	if *engine != store.EnginePostgres && *pgDSN != "" {
		err := fmt.Errorf("-pg-dsn applies only to -engine postgres")
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	mode, err := auth.ParseMode(*authMode)
	if err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}
	if maxGroupsErr != nil {
		fmt.Fprintf(out, "config: %v\n", maxGroupsErr)
		fs.Usage()
		return nil, &printedError{maxGroupsErr}
	}

	if err := auth.ValidateMaxGroups(*maxGroups); err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	proxies, err := auth.ParseTrustedProxies(*trustedProxies)
	if err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}
	oidc := auth.OIDCConfig{
		Issuer:       getenv("DOLMEN_AUTH_OIDC_ISSUER"),
		ClientID:     getenv("DOLMEN_AUTH_OIDC_CLIENT_ID"),
		ClientSecret: getenv("DOLMEN_AUTH_OIDC_CLIENT_SECRET"),
		Scopes:       auth.ParseScopes(getenv("DOLMEN_AUTH_OIDC_SCOPES")),
		GroupsClaim:  getenv("DOLMEN_AUTH_OIDC_GROUPS_CLAIM"),
		DeploymentID: getenv("DOLMEN_AUTH_OIDC_DEPLOYMENT_ID"),
		Preset:       getenv("DOLMEN_AUTH_OIDC_PRESET"),
		MaxGroups:    *maxGroups,
	}
	if raw := strings.TrimSpace(getenv("DOLMEN_AUTH_OIDC_TOKEN_TTL")); raw != "" {
		d, ttlErr := time.ParseDuration(raw)
		if ttlErr != nil {
			e := fmt.Errorf("invalid DOLMEN_AUTH_OIDC_TOKEN_TTL %q: %w", raw, ttlErr)
			fmt.Fprintf(out, "config: %v\n", e)
			fs.Usage()
			return nil, &printedError{e}
		}
		oidc.TokenTTL = d
	}
	if err := oidc.Validate(); err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	authn, err := auth.New(auth.Config{
		Mode:           mode,
		AdminKey:       getenv("DOLMEN_ADMIN_KEY"),
		TrustedProxies: proxies,
		MaxGroups:      *maxGroups,
		Stdio:          stdio,
	})
	if err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	retention, err := parseChangeRetention(*changeRetention)
	if err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	if maxOpenErr == nil && *maxOpenNamespaces < 1 {
		maxOpenErr = fmt.Errorf("invalid max open namespaces %d: must be at least 1", *maxOpenNamespaces)
	}
	if maxOpenErr != nil {
		fmt.Fprintf(out, "config: %v\n", maxOpenErr)
		fs.Usage()
		return nil, &printedError{maxOpenErr}
	}

	var maxAge time.Duration
	if !stdio {
		maxAge, err = parseMaxSubscriptionAge(*maxSubscriptionAge)
		if err != nil {
			fmt.Fprintf(out, "config: %v\n", err)
			fs.Usage()
			return nil, &printedError{err}
		}
	}

	timeouts, err := parseTimeouts(stdio, *readTimeout, *writeTimeout, *idleTimeout, *opTimeout, *migrateTimeout, *maxHeaderBytes, maxHeaderBytesErr)
	if err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	skillNamespaceHint := envOr("DOLMEN_SKILL_NAMESPACE_HINT", skill.DefaultNamespaceHint, getenv)
	prefixValue := skill.NormalizePrefix(*prefix)
	if *prefix != "" && prefixValue == "" {
		e := fmt.Errorf("-prefix %q is not a usable URL path prefix: use at most %d segments of [A-Za-z0-9._~:@-], at most %d bytes total, with no empty or dot segments", *prefix, skill.MaxPrefixSegments, skill.MaxPrefixBytes)
		fmt.Fprintf(out, "config: %v\n", e)
		fs.Usage()
		return nil, &printedError{e}
	}

	if *publicBaseURL != "" && prefixValue != "" {
		if strings.HasSuffix(strings.TrimRight(*publicBaseURL, "/"), prefixValue) {
			e := fmt.Errorf("-base-url %q already ends with -prefix %q: remove one of them, or set -base-url to the scheme://host part and let -prefix supply the path", *publicBaseURL, prefixValue)
			fmt.Fprintf(out, "%v\n", e)
			fs.Usage()
			return nil, &printedError{e}
		}
	}

	return &config{
		Addr:               *addr,
		DataDir:            *dataDir,
		Engine:             *engine,
		PostgresDSN:        *pgDSN,
		PostgresCatalog:    *pgCatalog,
		PostgresQueryRole:  *pgQueryRole,
		Auth:               authn,
		OIDC:               oidc,
		AllowedOrigins:     allowedOrigins,
		BaseURL:            *publicBaseURL,
		Prefix:             prefixValue,
		SkillNamespaceHint: skillNamespaceHint,
		ChangeRetention:    retention,
		MaxSubscriptionAge: maxAge,
		Timeouts:           timeouts,
		MaxOpenNamespaces:  *maxOpenNamespaces,
		Embed: embedConfig{
			Provider: provider,
			BaseURL:  baseURL,
			Model:    model,
			APIKey:   apiKey,
		},
		Version: *showVersion,
	}, nil
}

func parseChangeRetention(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid change retention %q: %w", raw, err)
	}
	if d == 0 || (d >= time.Hour && d <= 2160*time.Hour) {
		return d, nil
	}
	return 0, fmt.Errorf("invalid change retention %q: must be 0 (disable pruning) or between 1h and 2160h", raw)
}

func parseMaxSubscriptionAge(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid max subscription age %q: %w", raw, err)
	}
	if d == 0 || (d >= time.Second && d <= 24*time.Hour) {
		return d, nil
	}
	return 0, fmt.Errorf("invalid max subscription age %q: must be 0 (disable the bound) or between 1s and 24h", raw)
}

const (
	minHeaderBytes      = 4 << 10
	maxHeaderBytesLimit = 16 << 20
)

func parseTimeouts(stdio bool, read, write, idle, op, migrate string, headerBytes int, headerBytesErr error) (api.Timeouts, error) {
	var t api.Timeouts
	var err error
	if t.Op, err = parseBound("op timeout", op); err != nil {
		return t, err
	}
	if t.Migrate, err = parseBound("migrate timeout", migrate); err != nil {
		return t, err
	}
	if stdio {
		return t, nil
	}
	if t.Read, err = parseBound("read timeout", read); err != nil {
		return t, err
	}
	if t.Write, err = parseBound("write timeout", write); err != nil {
		return t, err
	}
	if t.Idle, err = parseBound("idle timeout", idle); err != nil {
		return t, err
	}
	if headerBytesErr != nil {
		return t, headerBytesErr
	}
	if headerBytes < minHeaderBytes || headerBytes > maxHeaderBytesLimit {
		return t, fmt.Errorf("invalid max header bytes %d: must be between %d and %d", headerBytes, minHeaderBytes, maxHeaderBytesLimit)
	}
	t.MaxHeaderBytes = headerBytes
	return t, nil
}

func parseBound(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	if d == 0 || (d >= time.Second && d <= 24*time.Hour) {
		return d, nil
	}
	return 0, fmt.Errorf("invalid %s %q: must be 0 (no bound) or between 1s and 24h", name, raw)
}

func envIntOr(key string, fallback int, getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be a whole number", key, raw)
	}
	return n, nil
}

func envOr(key, fallback string, getenv func(string) string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func printEnvHelp(out io.Writer) {
	help := []envHelp{
		{"DOLMEN_ADDR", "listen address (default 127.0.0.1:8790)"},
		{"DOLMEN_DATA", "data directory (default data)"},
		{"DOLMEN_ENGINE", "storage engine: sqlite (default) or postgres"},
		{"DOLMEN_PG_DSN", "PostgreSQL connection string, required with the postgres engine"},
		{"DOLMEN_PG_CATALOG", "PostgreSQL catalog schema (default dolmen_catalog)"},
		{"DOLMEN_PG_QUERY_ROLE", "pre-provisioned NOLOGIN role that caller SQL runs as"},
		{"DOLMEN_AUTH", "authentication: off (default) or on (deny-by-default)"},
		{"DOLMEN_ADMIN_KEY", "bootstrap admin credential, needed with auth on until a root administrator is granted (env-only, never a flag)"},
		{"DOLMEN_TRUSTED_PROXIES", "comma-separated CIDRs whose peers may assert identity headers"},
		{"DOLMEN_MAX_GROUPS", "maximum group entries accepted per request, 1 to 1024 (default 128)"},
		{"DOLMEN_AUTH_OIDC_ISSUER", "identity provider issuer URL, enabling native sign-in"},
		{"DOLMEN_AUTH_OIDC_CLIENT_ID", "OAuth client id registered with that provider"},
		{"DOLMEN_AUTH_OIDC_CLIENT_SECRET", "OAuth client secret (environment only)"},
		{"DOLMEN_AUTH_OIDC_SCOPES", "comma-separated extra scopes to request"},
		{"DOLMEN_AUTH_OIDC_GROUPS_CLAIM", "claim carrying the caller's groups (default groups)"},
		{"DOLMEN_AUTH_OIDC_TOKEN_TTL", "issued token lifetime, 1h to 720h (default 168h)"},
		{"DOLMEN_AUTH_OIDC_DEPLOYMENT_ID", "pin this deployment's token issuer id"},
		{"DOLMEN_AUTH_OIDC_PRESET", "github, to use GitHub instead of a generic OIDC provider"},
		{"DOLMEN_ALLOWED_ORIGINS", "comma-separated allowed HTTP origins for CORS"},
		{"DOLMEN_BASE_URL", "public base URL for skills and MCP links (default: use request Host)"},
		{"DOLMEN_SKILL_NAMESPACE_HINT", "hint text rendered into skill markdown"},
		{"DOLMEN_CHANGE_RETENTION", "change-log retention: 0 disables pruning, else 1h to 2160h (default 168h)"},
		{"DOLMEN_MAX_SUBSCRIPTION_AGE", "subscribe connection age bound: 0 disables, else 1s to 24h (default 30m)"},
		{"DOLMEN_READ_TIMEOUT", "time to read one request, headers and body: 0 disables, else 1s to 24h (default 2m)"},
		{"DOLMEN_WRITE_TIMEOUT", "time to write one response once it starts: 0 disables, else 1s to 24h (default 2m)"},
		{"DOLMEN_IDLE_TIMEOUT", "time a keep-alive connection may sit idle: 0 disables, else 1s to 24h (default 2m)"},
		{"DOLMEN_MAX_HEADER_BYTES", "largest request header block accepted, in bytes (default 1048576)"},
		{"DOLMEN_OP_TIMEOUT", "time for one operation's work: 0 disables, else 1s to 24h (default 2m)"},
		{"DOLMEN_MIGRATE_TIMEOUT", "time for one migrate call: 0 (default) is unbounded, else 1s to 24h"},
		{"DOLMEN_MAX_OPEN_NAMESPACES", "namespaces held open at once before idle ones are closed, at least 1 (default 128)"},
		{"", ""},
		{"DOLMEN_EMBED_PROVIDER", "embedding provider: none, local (default), or openai"},
		{"DOLMEN_EMBED_MODEL", "model name or absolute model-directory path"},
		{"DOLMEN_EMBED_BASE_URL", "base URL for an OpenAI-compatible provider"},
		{"DOLMEN_EMBED_API_KEY", "API key for an OpenAI-compatible provider"},
		{"OPENAI_API_KEY", "fallback API key when DOLMEN_EMBED_API_KEY is unset"},
		{"REMBED_CACHE", "model cache directory for the local provider"},
		{"HF_TOKEN", "Hugging Face token for gated repos downloaded by the local provider"},
	}

	fmt.Fprintln(out, "\nEnvironment variables:")
	for _, h := range help {
		if h.key == "" {
			fmt.Fprintln(out)
			continue
		}
		fmt.Fprintf(out, "  %s  %s\n", h.key, h.desc)
	}
}

func parseAllowedOrigins(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out, nil
}

func withPrefix(prefix string, next http.Handler) http.Handler {
	prefix = skill.NormalizePrefix(prefix)
	if prefix == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) || (len(r.URL.Path) > len(prefix) && r.URL.Path[len(prefix)] != '/') {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = r.URL.Path[len(prefix):]
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		if r2.URL.RawPath != "" {
			if strings.HasPrefix(r2.URL.RawPath, prefix) {
				r2.URL.RawPath = r2.URL.RawPath[len(prefix):]
				if r2.URL.RawPath == "" {
					r2.URL.RawPath = "/"
				}
			} else {
				r2.URL.RawPath = ""
			}
		}
		next.ServeHTTP(w, r2)
	})
}
