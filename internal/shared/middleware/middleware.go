package middleware

import "slices"

import "net/http"

func ChainMiddleware(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler {
	var final http.Handler = h
	for _, mw := range slices.Backward(mws) {
		final = mw(final)
	}
	return final
}
