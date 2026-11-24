package tenant

import "net/http"

// Resolver works out which tenant an HTTP request belongs to.
type Resolver interface {
	Resolve(r *http.Request) (ID, error)
}

// DefaultResolver resolves every request to the Default tenant (standalone).
type DefaultResolver struct{}

// Resolve always returns Default.
func (DefaultResolver) Resolve(*http.Request) (ID, error) { return Default, nil }

// Middleware puts the resolved tenant on the request context. Requests whose tenant can't be
// resolved get 404 "unknown tenant" and never reach the next handler.
func Middleware(res Resolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := res.Resolve(r)
			if err != nil || id == "" {
				http.Error(w, "unknown tenant", http.StatusNotFound)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithID(r.Context(), id)))
		})
	}
}
