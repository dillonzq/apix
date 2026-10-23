package gateway

import (
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/dillonzq/apix"
)

// Handler serves client requests in protocol p: it reads the body, calls Do,
// and writes the committed response, flushing after every read so streamed
// events reach the client promptly. Errors before the commit point are
// written in p's error format. After it, a failure ends the response early.
func (g *Gateway) Handler(p apix.Protocol) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			g.writeError(w, p, &apix.Error{Class: apix.ClassUnsupported, Message: "method not allowed", StatusCode: http.StatusMethodNotAllowed})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxRequestBytes))
		if err != nil {
			status := http.StatusBadRequest
			if errors.As(err, new(*http.MaxBytesError)) {
				status = http.StatusRequestEntityTooLarge
			}
			g.writeError(w, p, &apix.Error{Class: apix.ClassUnsupported, Message: "cannot read request body", StatusCode: status})
			return
		}
		payload := apix.Payload{Body: body, ContentType: r.Header.Get("Content-Type")}
		if spec, ok := g.reg.Spec(p); ok && spec.ParsePath != nil {
			metadata, err := spec.ParsePath(r.URL.Path)
			if err != nil {
				g.writeError(w, p, err)
				return
			}
			payload.Metadata = metadata
		}
		resp, derr := g.Do(r.Context(), Request{Protocol: p, Payload: payload})
		if derr != nil {
			g.writeError(w, p, derr)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			w.Header()[k] = vs
		}
		w.WriteHeader(resp.Status)
		rc := http.NewResponseController(w)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if resp.Stream {
					_ = rc.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	})
}

func (g *Gateway) writeError(w http.ResponseWriter, p apix.Protocol, err error) {
	payload := g.reg.EncodeError(p, err)
	w.Header().Set("Content-Type", contentType(payload.ContentType, "application/json"))
	if e := apix.AsError(err); e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	w.WriteHeader(apix.HTTPStatus(err))
	_, _ = w.Write(payload.Body)
}
