// Package errclass gives internal packages short names for the error
// classification defined in package apix.
package errclass

import "github.com/dillonzq/apix"

// Error is apix.Error.
type Error = apix.Error

// Class is apix.Class.
type Class = apix.Class

const (
	ClassAuth         = apix.ClassAuth
	ClassInvalidModel = apix.ClassInvalidModel
	ClassUnsupported  = apix.ClassUnsupported
	ClassRateLimit    = apix.ClassRateLimit
	ClassQuota        = apix.ClassQuota
	ClassBilling      = apix.ClassBilling
	ClassUpstream     = apix.ClassUpstream
	ClassNetwork      = apix.ClassNetwork
	ClassTranslation  = apix.ClassTranslation
)

// FromStatus is apix.FromStatus.
func FromStatus(status int, message string) *Error { return apix.FromStatus(status, message) }

// FromNetwork is apix.FromNetwork.
func FromNetwork(err error) *Error { return apix.FromNetwork(err) }

// PostFirstByteNetwork is apix.PostFirstByteNetwork.
func PostFirstByteNetwork(message string) *Error { return apix.PostFirstByteNetwork(message) }

// Translation is apix.Translation.
func Translation(message string) *Error { return apix.Translation(message) }

// UpstreamFallback is apix.UpstreamFallback.
func UpstreamFallback(message string) *Error { return apix.UpstreamFallback(message) }

// Redact is apix.Redact.
func Redact(message string) string { return apix.Redact(message) }
