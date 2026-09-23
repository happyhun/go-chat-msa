package httpio

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadJSON(t *testing.T) {
	const valid = `{"name":"room"}`
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "valid", body: valid},
		{name: "surrounding whitespace", body: " \n\t" + valid + "\r\n "},
		{name: "exact size limit", body: valid + strings.Repeat(" ", maxRequestBodySize-len(valid))},
		{name: "empty", wantErr: "request body must not be empty"},
		{name: "whitespace only", body: " \n\t", wantErr: "request body must not be empty"},
		{name: "incomplete", body: `{"name":`, wantErr: "request body contains badly-formed JSON"},
		{name: "syntax error", body: `{"name":!}`, wantErr: "request body contains badly-formed JSON (at position 9)"},
		{name: "wrong field type", body: `{"name":1}`, wantErr: `request body contains an invalid value for the "name" field (at position 9)`},
		{name: "unknown field", body: `{"other":1}`, wantErr: `json: unknown field "other"`},
		{name: "second object", body: valid + `{}`, wantErr: "request body must only contain a single JSON object"},
		{name: "trailing null", body: valid + "null", wantErr: "request body must only contain a single JSON object"},
		{name: "trailing garbage", body: valid + "x", wantErr: "request body must only contain a single JSON object"},
		{name: "oversized object", body: `{"name":"` + strings.Repeat("a", maxRequestBodySize) + `"}`, wantErr: "request body must not be larger than 1MB"},
		{name: "oversized trailing whitespace", body: valid + strings.Repeat(" ", maxRequestBodySize-len(valid)+1), wantErr: "request body must not be larger than 1MB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			var dst struct {
				Name string `json:"name"`
			}
			err := ReadJSON(req.Context(), rec, req, &dst)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, "room", dst.Name)
			}
			require.Empty(t, rec.Body.String())
		})
	}
}
