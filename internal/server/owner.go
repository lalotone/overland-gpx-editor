package server

import (
	"context"
	"errors"
	"log"
	"net/http"
)

// Owner is the opaque key under which a request's private data is stored.
//
// It is all the backend ever learns about who is asking. The serve wrapper
// derives it from the signed-in account with a secret the backend does not
// hold, so nothing here can be linked back to a username, an account ID or a
// passkey; the mobile host and an unauthenticated serve run as LocalOwner. It
// is never logged, never returned in a response and never put in telemetry.
type Owner string

// LocalOwner is the single owner of a server with no sign-in.
const LocalOwner Owner = "local"

// ownerKeyLength is the base32 length of a 32-byte derived key.
const ownerKeyLength = 52

// Valid reports whether o is LocalOwner or a derived key: 52 lower-case
// base32 characters. Both are safe as a directory name.
func (o Owner) Valid() bool {
	if o == LocalOwner {
		return true
	}
	if len(o) != ownerKeyLength {
		return false
	}
	for _, r := range o {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

type ownerContextKey struct{}
type operatorContextKey struct{}

// WithOwner marks a request as belonging to owner. The wrapper in front of
// the server calls it; handlers read the result with ownerOf.
func WithOwner(ctx context.Context, owner Owner) context.Context {
	return context.WithValue(ctx, ownerContextKey{}, owner)
}

// OwnerFrom returns the owner a request belongs to, if one was attached.
func OwnerFrom(ctx context.Context) (Owner, bool) {
	owner, ok := ctx.Value(ownerContextKey{}).(Owner)
	return owner, ok && owner.Valid()
}

// WithOperator marks a request as coming from an operator: someone allowed to
// change server-wide state such as the offline mode or the routing region.
func WithOperator(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorContextKey{}, true)
}

// IsOperator reports whether the request was marked with WithOperator.
func IsOperator(ctx context.Context) bool {
	operator, _ := ctx.Value(operatorContextKey{}).(bool)
	return operator
}

var errNoOwner = errors.New("request has no owner")

// attachOwner is the first middleware to run. Without RequireOwner every
// request belongs to LocalOwner; with it the wrapper must have attached one,
// and a request that reaches an owner-scoped route without it is a wiring
// bug, refused rather than served as the local owner.
func (s *Server) attachOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := OwnerFrom(r.Context()); !ok && !s.requireOwner {
			r = r.WithContext(WithOwner(r.Context(), LocalOwner))
		}
		next.ServeHTTP(w, r)
	})
}

// ownerOf returns the request's owner. Routes registered as ownerRoute or
// operatorRoute never run without one (see requireOwnerRoute), so the second
// result is only false on a public route under RequireOwner.
func ownerOf(r *http.Request) (Owner, bool) {
	return OwnerFrom(r.Context())
}

// requireOwnerRoute refuses a request that has no owner. It wraps every
// owner-scoped route, so a handler cannot forget to check.
func (s *Server) requireOwnerRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ownerOf(r); !ok {
			log.Printf("%s %s reached an owner-scoped route without an owner; the passkey wrapper is missing", r.Method, r.URL.Path)
			writeError(w, http.StatusInternalServerError, "Request has no owner")
			return
		}
		next(w, r)
	}
}
