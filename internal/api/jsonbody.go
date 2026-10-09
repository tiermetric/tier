package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// requireJSON refuses a POST, PUT or PATCH whose Content-Type media type is not
// application/json with 415 (#893). A browser sends a cross-site POST without a
// CORS preflight only when its Content-Type is text/plain, a form type, or
// absent; requiring application/json forces the preflight, which tierd never
// answers, so a page the operator visits cannot write. Parameters such as
// charset are allowed. DELETE is exempt: it carries no body on any route and a
// browser always preflights it.
func requireJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next(w, r)
	}
}

// requireJSONEOF reports an error unless dec's input ends after the value it
// has already decoded, apart from whitespace (#1042). Every write handler calls
// it after its one Decode, so a body is exactly one JSON value: a second value,
// a stray `]` or `}`, trailing garbage, or a read error (including
// *http.MaxBytesError when the trailing bytes push the body past its cap) is
// refused. json.Decoder.More cannot do this: it reports false at `]` and `}`.
func requireJSONEOF(dec *json.Decoder) error {
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected %v after the JSON value", tok)
}
