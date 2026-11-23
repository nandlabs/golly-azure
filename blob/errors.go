package blob

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"oss.nandlabs.io/golly/vfs"
)

// mapBlobErr translates an Azure Blob Storage SDK error into a golly/vfs
// sentinel so callers can switch on outcomes with errors.Is across
// backends. It returns nil for a nil input, and returns the original
// error wrapped with a matching sentinel when the Azure error code (or
// HTTP status) maps cleanly. If no mapping is known, the original error
// is returned unchanged so callers still see the SDK detail.
//
// Mapping:
//
//	BlobNotFound, ContainerNotFound, ResourceNotFound, HTTP 404 -> vfs.ErrNotExist
//	Authorization*/Authentication* codes, HTTP 401/403         -> vfs.ErrPermission
//	BlobAlreadyExists, ContainerAlreadyExists                   -> fs.ErrExist
//
// The returned error preserves the original chain (via fmt.Errorf %w),
// so errors.Is(err, vfs.ErrNotExist) AND errors.As(err, **azcore.ResponseError)
// both continue to work.
func mapBlobErr(err error) error {
	if err == nil {
		return nil
	}

	// Prefer the service error code — stable across API versions,
	// unlike HTTP status which is shared by many distinct conditions.
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound,
		bloberror.ResourceNotFound):
		return fmt.Errorf("%w: %w", vfs.ErrNotExist, err)
	case bloberror.HasCode(err, bloberror.AuthorizationFailure, bloberror.AuthorizationPermissionMismatch,
		bloberror.AuthorizationProtocolMismatch, bloberror.AuthorizationResourceTypeMismatch,
		bloberror.AuthorizationServiceMismatch, bloberror.AuthorizationSourceIPMismatch,
		bloberror.AuthenticationFailed, bloberror.InvalidAuthenticationInfo,
		bloberror.NoAuthenticationInformation, bloberror.InsufficientAccountPermissions,
		bloberror.AccountIsDisabled):
		return fmt.Errorf("%w: %w", vfs.ErrPermission, err)
	case bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ContainerAlreadyExists):
		return fmt.Errorf("%w: %w", fs.ErrExist, err)
	}

	// Fallback: HEAD requests carry no body, and some proxies / emulators
	// omit x-ms-error-code. Map on the HTTP status instead.
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", vfs.ErrNotExist, err)
		case http.StatusForbidden, http.StatusUnauthorized:
			return fmt.Errorf("%w: %w", vfs.ErrPermission, err)
		}
	}

	return err
}

// isNotFound reports whether err denotes a missing blob or container.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound, bloberror.ResourceNotFound) {
		return true
	}
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

// isAlreadyExists reports whether err is the result of a failed
// If-None-Match:* precondition (Azure answers 409 BlobAlreadyExists, some
// emulators 412 ConditionNotMet).
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet) {
		return true
	}
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) &&
		(respErr.StatusCode == http.StatusConflict || respErr.StatusCode == http.StatusPreconditionFailed)
}

// isInvalidRange reports whether err is an HTTP 416 (range not satisfiable).
func isInvalidRange(err error) bool {
	if err == nil {
		return false
	}
	if bloberror.HasCode(err, bloberror.InvalidRange) {
		return true
	}
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusRequestedRangeNotSatisfiable
}
