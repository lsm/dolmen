package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/files"
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
		writeError(w, r, badRequest("the first multipart part must be named \"request\" and hold the operation's JSON request, as curl -F 'request=<json>' -F 'name=@path'; it was named %q", first.FormName()))
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
	return badRequest("%s: %s", msg, err)
}
