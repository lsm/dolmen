package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

const maxRequestPartBytes = 32 << 20

var fileWriteOps = map[string]bool{
	"insert":        true,
	"update":        true,
	"upsert":        true,
	"upsert_by_key": true,
	"batch":         true,
}

func WithFiles(fs *files.Store) Option {
	return func(s *Server) {
		s.files = fs
	}
}

func isMultipart(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mt, "multipart/form-data")
}

type idleReader struct {
	r   io.Reader
	rc  *http.ResponseController
	idl time.Duration
}

func (d idleReader) Read(p []byte) (int, error) {
	if d.idl > 0 {
		_ = d.rc.SetReadDeadline(time.Now().Add(d.idl))
	}
	return d.r.Read(p)
}

func (s *Server) multipartOp(w http.ResponseWriter, r *http.Request, op string) {
	if !fileWriteOps[op] {
		writeError(w, r, &Error{Status: http.StatusUnsupportedMediaType, Code: ErrCodeInvalid, Message: fmt.Sprintf("%s takes an application/json body; multipart/form-data is accepted only by insert, update, upsert, upsert_by_key and batch, to carry files", op)})
		return
	}
	if s.files == nil {
		writeError(w, r, badRequest("this server does not store files"))
		return
	}
	rc := http.NewResponseController(w)
	r.Body = io.NopCloser(idleReader{r: r.Body, rc: rc, idl: s.timeouts.Read})
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, r, badRequest("cannot read the multipart body: %s", err))
		return
	}
	first, err := mr.NextPart()
	if err != nil {
		s.ArmResponseWrite(w)
		writeError(w, r, s.partReadError(err, "the multipart body has no parts; the first part must be named \"request\" and hold the operation's JSON request"))
		return
	}
	if first.FormName() != "request" {
		s.ArmResponseWrite(w)
		writeError(w, r, badRequest("the first multipart part must be named \"request\" and hold the operation's JSON request, as curl --form-string 'request=<json>' -F 'name=@path'; it was named %q", first.FormName()))
		return
	}
	body, err := io.ReadAll(io.LimitReader(first, maxRequestPartBytes+1))
	if err != nil {
		s.ArmResponseWrite(w)
		writeError(w, r, s.partReadError(err, "cannot read the request part"))
		return
	}
	if len(body) > maxRequestPartBytes {
		s.ArmResponseWrite(w)
		writeError(w, r, &Error{Status: http.StatusRequestEntityTooLarge, Code: ErrCodeInvalid, Message: "the request part exceeds the 32 MiB limit; files go in their own parts"})
		return
	}
	if err := s.authorizeOp(r.Context(), op, body); err != nil {
		s.ArmResponseWrite(w)
		writeError(w, r, err)
		return
	}
	req := s.files.NewRequest()
	defer req.Close()
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.ArmResponseWrite(w)
			writeError(w, r, s.partReadError(err, "cannot read the multipart body"))
			return
		}
		if err := req.Receive(p.FormName(), p.FileName(), p.Header.Get("Content-Type"), p); err != nil {
			s.ArmResponseWrite(w)
			writeError(w, r, s.partReadError(err, "cannot read file part "+strconv.Quote(p.FormName())))
			return
		}
	}
	ctx := store.WithFileSource(r.Context(), req)
	res, err := s.Dispatch(ctx, op, body)
	s.ArmResponseWrite(w)
	if err != nil {
		if code := WrapError(err).Code; code != ErrCodeTimeout && code != ErrCodeCanceled {
			req.Discard()
		}
		slog.DebugContext(r.Context(), "op failed", WithPrincipal(r, "op", op, "err", err)...)
		writeError(w, r, err)
		return
	}
	payload, err := EncodeJSON(map[string]any{"ok": true, "data": res})
	if err != nil {
		writeError(w, r, internal(err))
		return
	}
	WriteJSONPayload(w, http.StatusOK, payload)
}

func (s *Server) partReadError(err error, msg string) error {
	var fe *files.Error
	if errors.As(err, &fe) {
		return err
	}
	if bodyReadTimedOut(err) {
		return &Error{Status: http.StatusRequestTimeout, Code: ErrCodeTimeout, Message: fmt.Sprintf("the request body stalled for longer than the server's %s read limit (-read-timeout, DOLMEN_READ_TIMEOUT) between chunks; retry the write", s.timeouts.Read)}
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return internal(err)
	}
	return &Error{Status: http.StatusBadRequest, Code: ErrCodeInvalid, Message: msg, Cause: err}
}

type idleWriter struct {
	http.ResponseWriter
	rc  *http.ResponseController
	idl time.Duration
}

func (d idleWriter) Write(p []byte) (int, error) {
	if d.idl > 0 {
		_ = d.rc.SetWriteDeadline(time.Now().Add(d.idl))
	}
	return d.ResponseWriter.Write(p)
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(WithRequestID(r.Context(), RequestIDFor(r)))
	w.Header().Set("X-Request-Id", RequestIDFrom(r.Context()))
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET to download a file; files are attached by sending them with a write, as multipart"})
		return
	}
	qv := r.URL.Query()
	signed := qv.Has("sig")
	if !signed {
		authed, err := s.Authenticated(r)
		r = authed
		if err != nil {
			writeError(w, r, WrapError(err))
			return
		}
	}
	if s.files == nil {
		writeError(w, r, notFound("this server does not store files"))
		return
	}
	ns, table, field, idText := qv.Get("namespace"), qv.Get("table"), qv.Get("field"), qv.Get("id")
	if ns == "" || table == "" || field == "" || idText == "" {
		writeError(w, r, badRequest("download a file with GET /v1/files?namespace=<ns>&table=<table>&id=<row id>&field=<file field>"))
		return
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 {
		writeError(w, r, badRequest("id must be a positive row id"))
		return
	}
	if signed {
		ctx, err := s.verifySignedDownload(r.Context(), qv, normNS(ns), normTable(table), id, field, time.Now())
		if err != nil {
			writeError(w, r, err)
			return
		}
		r = r.WithContext(ctx)
	}
	body, _ := json.Marshal(map[string]any{"namespace": ns, "table": table, "ids": []int64{id}})
	ref, err := s.fileRefFor(r.Context(), normNS(ns), normTable(table), id, field)
	if signed && linkNoLongerMatches(err, ref, qv.Get("sha256")) {
		err = invalidLink()
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if s.beforeFileOpen != nil {
		s.beforeFileOpen()
	}
	f, err := s.files.Open(r.Context(), normNS(ns), ref.sha)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			again, rerr := s.Dispatch(r.Context(), "read_rows", body)
			if rerr != nil {
				writeError(w, r, rerr)
				return
			}
			now, rerr := fileRefFrom(again, table, id, field)
			if rerr != nil {
				writeError(w, r, rerr)
				return
			}
			if now.sha != ref.sha {
				writeError(w, r, notFound("the file of row %d changed while it was being downloaded; download it again", id))
				return
			}
			writeError(w, r, internal(fmt.Errorf("file %s of %s row %d field %s is missing from the file store: %w", ref.sha, table, id, field, err)))
			return
		}
		var fe *files.Error
		if errors.As(err, &fe) {
			writeError(w, r, err)
			return
		}
		writeError(w, r, internal(err))
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", ref.mediaType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Content-Disposition", contentDisposition(ref.name, ref.sha))
	h.Set("ETag", `"`+ref.sha+`"`)
	h.Set("Cache-Control", "private, no-cache")
	idle := idleWriter{ResponseWriter: w, rc: http.NewResponseController(w), idl: s.timeouts.Write}
	http.ServeContent(idle, r, "", time.Time{}, f)
}

type fileRef struct {
	sha       string
	mediaType string
	name      string
}

func fileRefFrom(res any, table string, id int64, field string) (fileRef, error) {
	out, _ := res.(map[string]any)
	rows, _ := out["rows"].([]map[string]any)
	if len(rows) == 0 {
		return fileRef{}, notFound("table %s has no row %d", table, id)
	}
	v, ok := rows[0][field]
	if !ok {
		return fileRef{}, notFound("table %s has no field %q", table, field)
	}
	if v == nil {
		return fileRef{}, notFound("row %d of %s holds no file in %q", id, table, field)
	}
	m, isMap := v.(map[string]any)
	sha, _ := m["sha256"].(string)
	if !isMap || !files.ValidHash(sha) {
		return fileRef{}, badRequest("field %q of %s is not a file field", field, table)
	}
	ref := fileRef{sha: sha, mediaType: "application/octet-stream"}
	if mt, ok := m["media_type"].(string); ok && mt != "" {
		ref.mediaType = mt
	}
	if n, ok := m["name"].(string); ok {
		ref.name = n
	}
	return ref, nil
}

func contentDisposition(name, sha string) string {
	if name == "" {
		return `attachment; filename="` + sha + `"`
	}
	var ascii strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii.WriteByte('_')
			continue
		}
		ascii.WriteRune(r)
	}
	return `attachment; filename="` + ascii.String() + `"; filename*=UTF-8''` + rfc5987(name)
}

func rfc5987(s string) string {
	const attrChar = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(attrChar, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func (s *Server) copyReach(ctx context.Context, ns, table string) (*store.RowScope, error) {
	body, err := json.Marshal(map[string]any{"namespace": ns, "table": table})
	if err != nil {
		return nil, err
	}
	if err := s.authorizeOp(ctx, "read_rows", body); err != nil {
		return nil, err
	}
	scope, _, err := s.resolveScope(ctx, ns, table)
	return scope, err
}

const (
	defaultFileURLTTL = 900
	maxFileURLTTL     = 3600
)

type publicBaseKey struct{}

func WithPublicBase(ctx context.Context, base func() (string, error)) context.Context {
	return context.WithValue(ctx, publicBaseKey{}, base)
}

func (s *Server) PublicBase(r *http.Request) func() (string, error) {
	return func() (string, error) {
		if !s.UsableHost(r) {
			return "", errUnusableHost
		}
		return s.publicContext(r).BaseURL, nil
	}
}

func publicBaseFrom(ctx context.Context) (string, error) {
	if f, ok := ctx.Value(publicBaseKey{}).(func() (string, error)); ok {
		return f()
	}
	return "", badRequest("file_url links to this server's HTTP download endpoint, which this transport does not serve; call file_url over HTTP (/v1/file_url or /mcp)")
}

type fileURLReq struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
	ID        int64  `json:"id"`
	Field     string `json:"field"`
	TTL       *int64 `json:"ttl_seconds"`
}

var fileURLOp = OpDef{
	Description: "Get a short-lived URL that downloads one row's file with a plain GET and no credentials, to hand to a tool that holds none. It needs the same access as reading the row with read_rows. The URL stops working at expires_at; on a server that keeps files locally it also stops as soon as the row's file is replaced, while on S3 it is the bucket's own link and works until it expires.",
	InputSchema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"namespace":   nsProp("Namespace of the table"),
			"table":       existingTableProp("Table name"),
			"id":          map[string]any{"type": "integer", "minimum": 1, "description": "Row id"},
			"field":       prop("string", "Name of the file field"),
			"ttl_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxFileURLTTL, "default": defaultFileURLTTL, "description": "Seconds until the URL expires"},
		},
		"required": []string{"namespace", "table", "id", "field"},
	},
	OutputSchema: outSchema(map[string]any{
		"url":        prop("string", "Download URL; fetch it with curl -o path '<url>'"),
		"expires_at": map[string]any{"type": "string", "format": "date-time", "description": "When the URL stops working"},
	}, "url", "expires_at"),
	Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
		var req fileURLReq
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		var base string
		if s.files == nil || !s.files.Presigns() {
			b, err := publicBaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			base = b
		}
		if s.files == nil {
			return nil, badRequest("this server does not store files")
		}
		ttl := int64(defaultFileURLTTL)
		if req.TTL != nil {
			if *req.TTL < 1 || *req.TTL > maxFileURLTTL {
				return nil, badRequest("ttl_seconds must be between 1 and %d", maxFileURLTTL)
			}
			ttl = *req.TTL
		}
		if req.ID < 1 {
			return nil, badRequest("id must be a positive row id")
		}
		ns, table := normNS(req.Namespace), normTable(req.Table)
		ref, err := s.fileRefFor(ctx, ns, table, req.ID, req.Field)
		if err != nil {
			return nil, err
		}
		exp := time.Now().Add(time.Duration(ttl) * time.Second).Truncate(time.Second)
		if s.files.Presigns() {
			link, err := s.files.PresignGet(ctx, ns, ref.sha, time.Duration(ttl)*time.Second, ref.mediaType, contentDisposition(ref.name, ref.sha))
			if err != nil {
				return nil, err
			}
			return map[string]any{"url": link, "expires_at": exp.UTC().Format(time.RFC3339)}, nil
		}
		sig, err := s.files.Signer().Sign(ns, table, req.ID, req.Field, ref.sha, exp.Unix())
		if err != nil {
			return nil, internal(err)
		}
		q := url.Values{}
		q.Set("namespace", ns)
		q.Set("table", table)
		q.Set("id", strconv.FormatInt(req.ID, 10))
		q.Set("field", req.Field)
		q.Set("sha256", ref.sha)
		q.Set("exp", strconv.FormatInt(exp.Unix(), 10))
		q.Set("sig", sig)
		return map[string]any{"url": base + "/v1/files?" + q.Encode(), "expires_at": exp.UTC().Format(time.RFC3339)}, nil
	},
}

func (s *Server) fileRefFor(ctx context.Context, ns, table string, id int64, field string) (fileRef, error) {
	body, _ := json.Marshal(map[string]any{"namespace": ns, "table": table, "ids": []int64{id}})
	res, err := s.Dispatch(ctx, "read_rows", body)
	if err != nil {
		return fileRef{}, err
	}
	ref, err := fileRefFrom(res, table, id, field)
	if err != nil {
		return fileRef{}, err
	}
	sc, _, err := s.eng.TableState(ctx, ns, table, nil)
	if err != nil {
		return fileRef{}, wrapStoreErr(err)
	}
	if f := sc.Field(field); f == nil || f.Type != schema.File {
		return fileRef{}, badRequest("field %q of %s is not a file field", field, table)
	}
	return ref, nil
}

func linkNoLongerMatches(err error, ref fileRef, sum string) bool {
	if err == nil {
		return ref.sha != sum
	}
	code := WrapError(err).Code
	return code == ErrCodeNotFound || code == ErrCodeInvalid
}

func invalidLink() error {
	return forbidden("this download link is invalid, or the row's file has changed since it was issued; call file_url again for a new link")
}

func (s *Server) verifySignedDownload(ctx context.Context, qv url.Values, ns, table string, id int64, field string, now time.Time) (context.Context, error) {
	exp, err := strconv.ParseInt(qv.Get("exp"), 10, 64)
	if err != nil {
		return nil, invalidLink()
	}
	if now.Unix() > exp {
		return nil, forbidden("this download link expired at %s; call file_url again for a new link", time.Unix(exp, 0).UTC().Format(time.RFC3339))
	}
	sum := qv.Get("sha256")
	if !files.ValidHash(sum) {
		return nil, invalidLink()
	}
	ok, err := s.files.Signer().Valid(ns, table, id, field, sum, exp, qv.Get("sig"))
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, invalidLink()
	}
	return auth.WithIdentity(ctx, auth.Identity{Principal: auth.AdminPrincipal, Source: "file-url"}), nil
}

func init() {
	Ops["file_url"] = fileURLOp
}
