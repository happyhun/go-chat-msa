package middleware

import "net/http"

func ChainMiddleware(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler {
	var final http.Handler = h
	for i := len(mws) - 1; i >= 0; i-- {
		final = mws[i](final)
	}
	return final
}
