package shared

import (
	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/vendorerr"
)

// RedactedSnippet bearer-redacts and truncates a payload snippet for error
// messages — never an upstream body echo.
func RedactedSnippet(s string) string { return vendorerr.Snippet(s) }

// UpstreamStatusError classifies an upstream >=400 response body with the
// kernel every Kind shares; see vendorerr.UpstreamStatus.
func UpstreamStatusError(status int, body []byte) *errclass.Error {
	return vendorerr.UpstreamStatus(status, body)
}
