package plugins

import (
	"errors"
	"fmt"
)

// PackageError is an install or update refused because of the plugin package
// itself or the catalog that lists it: a failed download, a checksum
// mismatch, an unsupported platform or API version, or a binary that is not
// the plugin its catalog lists. Message is written for the admin who asked for
// the install and names no URL or checksum; Err keeps the detail for logs.
// Transient marks a failure to reach the catalog or download host, which the
// same request may get past later; every other PackageError recurs until the
// package or catalog changes.
type PackageError struct {
	Message   string
	Transient bool
	Err       error
}

func (e *PackageError) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *PackageError) Unwrap() error { return e.Err }

func packageError(err error, format string, args ...any) error {
	return &PackageError{Message: fmt.Sprintf(format, args...), Err: err}
}

func transientPackageError(err error, format string, args ...any) error {
	return &PackageError{Message: fmt.Sprintf(format, args...), Transient: true, Err: err}
}

// AsPackageError returns err's PackageError, if it has one.
func AsPackageError(err error) (*PackageError, bool) {
	var packageErr *PackageError
	if !errors.As(err, &packageErr) {
		return nil, false
	}
	return packageErr, true
}

func unsupportedAPIVersionError(version, supported string) error {
	return packageError(fmt.Errorf("plugin silo_api_version %q is not supported", version),
		"This plugin needs Silo plugin API %q, and this server supports %q.", version, supported)
}
