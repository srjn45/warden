package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/srjn45/warden/internal/store"
)

// apiError drives a specific HTTP status + the daemon's standard {"error": msg}
// envelope from a strict handler. Documented error codes the spec does not model
// as a typed response (403/409/422/…) funnel through here, so the wire body stays
// byte-identical to the hand-written writeErr path the daemon has always used.
type apiError struct {
	code int
	msg  string
}

func (e apiError) Error() string { return e.msg }

// errStatus builds an apiError; handlers return it as the error result of a
// strict method to emit code + {"error": msg}.
func errStatus(code int, msg string) apiError { return apiError{code: code, msg: msg} }

// strictResponseError renders an error returned by a strict handler as the
// daemon's standard JSON error envelope. An apiError carries an explicit status;
// any other error is an unexpected internal error (500).
func strictResponseError(w http.ResponseWriter, _ *http.Request, err error) {
	code := http.StatusInternalServerError
	msg := err.Error()
	var ae apiError
	if errors.As(err, &ae) {
		code, msg = ae.code, ae.msg
	}
	writeJSON(w, code, errorResponse{Error: msg})
}

// strictRequestError maps a request-body decode failure to the daemon's
// historical 400 {"error":"bad json"} instead of the generator's plain-text 400.
func strictRequestError(w http.ResponseWriter, _ *http.Request, _ error) {
	writeErr(w, http.StatusBadRequest, "bad json")
}

// strictParamError renders a query/path parameter binding failure (a missing
// required param or a bad format) as the daemon's JSON error envelope with 400,
// instead of the generator's default plain-text response.
func strictParamError(w http.ResponseWriter, _ *http.Request, err error) {
	writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
}

// requestCtxKey carries the live *http.Request into strict handlers, which only
// receive a context.Context. recordAudit/clientIP need the request for the
// caller's origin IP.
type requestCtxKey struct{}

// stashRequest is chi middleware that stores the *http.Request on the request
// context so strict handlers can recover it (see requestFromContext). It is
// mounted on the authenticated /api/v1 group alongside the generated handler.
func stashRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestCtxKey{}, r)))
	})
}

// requestFromContext recovers the request stashed by stashRequest. Under the
// daemon router it is always present; the empty-request fallback keeps
// clientIP panic-free for tests that invoke strict handlers without the
// middleware.
func requestFromContext(ctx context.Context) *http.Request {
	if r, ok := ctx.Value(requestCtxKey{}).(*http.Request); ok && r != nil {
		return r
	}
	return &http.Request{}
}

// recordAuditCtx is recordAudit for strict handlers: it recovers the request from
// ctx so the audit actor (caller IP) is still stamped.
func (s *Server) recordAuditCtx(ctx context.Context, action, target string, detail map[string]string) {
	s.recordAudit(requestFromContext(ctx), action, target, detail)
}

// derefSessions converts the store's []*store.Session into the value slice the
// generated SessionList holds. It preserves nil-ness so the wire output matches
// the hand-written handlers byte for byte (a nil input still marshals to null,
// an empty input to []).
func derefSessions(in []*store.Session) []store.Session {
	if in == nil {
		return nil
	}
	out := make([]store.Session, 0, len(in))
	for _, ss := range in {
		out = append(out, *ss)
	}
	return out
}
