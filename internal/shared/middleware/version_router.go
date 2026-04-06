package middleware

import "net/http"

type VersionRouter struct {
	muxes    map[string]http.Handler
	fallback http.Handler
}

func NewVersionRouter(fallback http.Handler, versions map[string]http.Handler) *VersionRouter {
	return &VersionRouter{
		muxes:    versions,
		fallback: fallback,
	}
}

func (vr *VersionRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	version := r.Header.Get("X-API-Version")
	if mux, ok := vr.muxes[version]; ok {
		mux.ServeHTTP(w, r)
		return
	}
	vr.fallback.ServeHTTP(w, r)
}
