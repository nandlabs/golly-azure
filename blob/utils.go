package blob

import (
	"errors"
	"net/url"
	"strings"

	"oss.nandlabs.io/golly/textutils"
)

const (
	// BlobScheme is the URL scheme for Azure Blob Storage resources.
	BlobScheme = "azblob"
)

// urlOpts holds parsed Azure Blob URL components.
type urlOpts struct {
	u *url.URL
	// Account is the storage account name (URL host).
	Account string
	// Container is the blob container name (first path segment).
	Container string
	// Key is the blob name within the container (remaining path, no leading slash).
	Key string
}

// parseURL parses an Azure Blob URL into its account, container and key components.
// Expected format: azblob://<storage-account>/<container>/<path/to/blob>
func parseURL(u *url.URL) (opts *urlOpts, err error) {
	if err = validateURL(u); err != nil {
		return
	}

	p := strings.TrimPrefix(u.Path, textutils.ForwardSlashStr)
	container, key, _ := strings.Cut(p, textutils.ForwardSlashStr)
	if container == "" {
		return nil, errors.New("invalid azblob URL, container name (first path segment) is required")
	}

	opts = &urlOpts{
		u:         u,
		Account:   u.Host,
		Container: container,
		Key:       key,
	}
	return
}

// validateURL checks that the URL is a valid azblob URL.
func validateURL(u *url.URL) error {
	if u == nil {
		return errors.New("url cannot be nil")
	}
	if u.Scheme != BlobScheme {
		return errors.New("invalid URL scheme, expected 'azblob'")
	}
	if u.Host == "" {
		return errors.New("invalid azblob URL, storage account name (host) is required")
	}
	return nil
}

// blobURL builds the azblob:// URL for key in the same account and container as opts.
func (o *urlOpts) blobURL(key string) *url.URL {
	return &url.URL{
		Scheme: BlobScheme,
		Host:   o.Account,
		Path:   "/" + o.Container + "/" + key,
	}
}

// withKey returns a copy of opts pointing at key in the same container.
func (o *urlOpts) withKey(key string) *urlOpts {
	return &urlOpts{
		u:         o.blobURL(key),
		Account:   o.Account,
		Container: o.Container,
		Key:       key,
	}
}

// isDirKey reports whether key denotes a directory (container root or trailing slash).
func isDirKey(key string) bool {
	return key == "" || strings.HasSuffix(key, textutils.ForwardSlashStr)
}

// dirPrefix returns key normalised as a listing prefix: "" stays "", and any
// other key gets a trailing slash.
func dirPrefix(key string) string {
	if key != "" && !strings.HasSuffix(key, textutils.ForwardSlashStr) {
		return key + textutils.ForwardSlashStr
	}
	return key
}
