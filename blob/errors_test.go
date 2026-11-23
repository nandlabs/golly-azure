package blob

import (
	"errors"
	"io/fs"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"oss.nandlabs.io/golly/vfs"
)

// respErr builds a synthetic azcore.ResponseError so mapBlobErr can be
// driven without a live service.
func respErr(status int, code string) error {
	return &azcore.ResponseError{StatusCode: status, ErrorCode: code}
}

func TestMapBlobErr_Nil(t *testing.T) {
	if err := mapBlobErr(nil); err != nil {
		t.Errorf("mapBlobErr(nil) = %v, want nil", err)
	}
}

func TestMapBlobErr_NotFoundCodes(t *testing.T) {
	for _, code := range []string{"BlobNotFound", "ContainerNotFound", "ResourceNotFound"} {
		src := respErr(http.StatusNotFound, code)
		got := mapBlobErr(src)
		if !errors.Is(got, vfs.ErrNotExist) {
			t.Errorf("%s did not map to vfs.ErrNotExist: %v", code, got)
		}
		if !errors.Is(got, fs.ErrNotExist) {
			t.Errorf("%s: mapped error should still satisfy fs.ErrNotExist", code)
		}
		var re *azcore.ResponseError
		if !errors.As(got, &re) {
			t.Errorf("%s: wrapped error should still be an *azcore.ResponseError", code)
		}
	}
}

func TestMapBlobErr_PermissionCodes(t *testing.T) {
	for _, code := range []string{
		"AuthorizationFailure", "AuthorizationPermissionMismatch", "AuthenticationFailed",
		"InvalidAuthenticationInfo", "NoAuthenticationInformation", "AccountIsDisabled",
	} {
		got := mapBlobErr(respErr(http.StatusForbidden, code))
		if !errors.Is(got, vfs.ErrPermission) || !errors.Is(got, fs.ErrPermission) {
			t.Errorf("%s did not map to vfs.ErrPermission: %v", code, got)
		}
	}
}

func TestMapBlobErr_AlreadyExists(t *testing.T) {
	for _, code := range []string{"BlobAlreadyExists", "ContainerAlreadyExists"} {
		if got := mapBlobErr(respErr(http.StatusConflict, code)); !errors.Is(got, fs.ErrExist) {
			t.Errorf("%s did not map to fs.ErrExist: %v", code, got)
		}
	}
}

func TestMapBlobErr_StatusFallback(t *testing.T) {
	if got := mapBlobErr(respErr(http.StatusNotFound, "")); !errors.Is(got, vfs.ErrNotExist) {
		t.Errorf("HTTP 404 without code did not map to ErrNotExist: %v", got)
	}
	for _, st := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		if got := mapBlobErr(respErr(st, "")); !errors.Is(got, vfs.ErrPermission) {
			t.Errorf("HTTP %d without code did not map to ErrPermission: %v", st, got)
		}
	}
}

func TestMapBlobErr_UnknownPassthrough(t *testing.T) {
	for _, src := range []error{respErr(http.StatusInternalServerError, "InternalError"), errors.New("random")} {
		got := mapBlobErr(src)
		if errors.Is(got, vfs.ErrNotExist) || errors.Is(got, vfs.ErrPermission) {
			t.Errorf("unknown error should not map to a sentinel: %v", got)
		}
		if got != src {
			t.Error("unknown error should be returned unchanged (same instance)")
		}
	}
}

func TestErrorPredicates(t *testing.T) {
	if isNotFound(nil) || isAlreadyExists(nil) || isInvalidRange(nil) {
		t.Error("predicates must be false for nil")
	}
	if !isNotFound(respErr(http.StatusNotFound, "")) || isNotFound(errors.New("x")) {
		t.Error("isNotFound")
	}
	if !isAlreadyExists(respErr(http.StatusConflict, "BlobAlreadyExists")) ||
		!isAlreadyExists(respErr(http.StatusPreconditionFailed, "")) ||
		isAlreadyExists(respErr(http.StatusNotFound, "BlobNotFound")) {
		t.Error("isAlreadyExists")
	}
	if !isInvalidRange(respErr(http.StatusRequestedRangeNotSatisfiable, "InvalidRange")) ||
		!isInvalidRange(respErr(http.StatusRequestedRangeNotSatisfiable, "")) ||
		isInvalidRange(errors.New("x")) {
		t.Error("isInvalidRange")
	}
}
