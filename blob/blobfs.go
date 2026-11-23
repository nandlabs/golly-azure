package blob

import (
	"context"
	"net/url"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"oss.nandlabs.io/golly/vfs"
)

var fsSchemes = []string{BlobScheme}

// BlobFS implements the vfs.VFileSystem interface for Azure Blob Storage.
//
// URLs take the form azblob://<storage-account>/<container>/<path/to/blob>.
// Every method has a context-aware counterpart (see VFileSystemCtx); the
// plain variants call it with context.Background().
type BlobFS struct {
	*vfs.BaseVFS
}

// Schemes returns the URL schemes supported by this filesystem.
func (fs *BlobFS) Schemes() []string {
	return fsSchemes
}

// newFile creates a BlobFile bound to this filesystem.
func (fs *BlobFS) newFile(client *container.Client, opts *urlOpts) *BlobFile {
	return newBlobFile(client, fs, opts)
}

// Create creates a new empty blob at the given URL. It fails (wrapping
// fs.ErrExist) if the blob already exists; the existence check is atomic
// (If-None-Match: *).
func (fs *BlobFS) Create(u *url.URL) (vfs.VFile, error) {
	return fs.CreateCtx(context.Background(), u)
}

// Mkdir creates a directory marker (blob name ending with "/"). It is an
// alias for MkdirAll since Azure Blob Storage directories are virtual.
func (fs *BlobFS) Mkdir(u *url.URL) (vfs.VFile, error) {
	return fs.MkdirAll(u)
}

// MkdirAll creates a zero-byte directory marker blob ("<key>/"). When the
// URL names the container root, the container is created instead. A
// missing container is created on demand.
func (fs *BlobFS) MkdirAll(u *url.URL) (vfs.VFile, error) {
	return fs.MkdirAllCtx(context.Background(), u)
}

// Open opens the blob at the given URL. It does not validate existence.
func (fs *BlobFS) Open(u *url.URL) (vfs.VFile, error) {
	return fs.OpenCtx(context.Background(), u)
}

// Copy copies a blob from src to dst. If src is a directory, every blob
// under it is copied recursively. Same-account copies are server-side
// (Copy Blob); cross-account copies fall back to streaming.
func (fs *BlobFS) Copy(src, dst *url.URL) error {
	return fs.CopyCtx(context.Background(), src, dst)
}

// Delete deletes the blob at the given URL. If it is a directory, every
// blob under it is deleted.
func (fs *BlobFS) Delete(src *url.URL) error {
	return fs.DeleteCtx(context.Background(), src)
}

// List lists the direct children (blobs and virtual directories) of the
// given prefix.
func (fs *BlobFS) List(u *url.URL) ([]vfs.VFile, error) {
	return fs.ListCtx(context.Background(), u)
}

// Walk traverses every blob under the prefix (flat listing), calling fn
// for each. vfs.ErrSkipAll stops the walk without error; vfs.ErrSkipDir
// returned for a directory marker skips its subtree, and returned for a
// file skips the remaining entries of that file's directory.
func (fs *BlobFS) Walk(u *url.URL, fn vfs.WalkFn) error {
	return fs.WalkCtx(context.Background(), u, fn)
}

// Move moves a blob (or directory) from src to dst (copy + delete).
func (fs *BlobFS) Move(src, dst *url.URL) error {
	return fs.MoveCtx(context.Background(), src, dst)
}

// Find finds files under the given location that match the filter.
func (fs *BlobFS) Find(location *url.URL, filter vfs.FileFilter) ([]vfs.VFile, error) {
	var files []vfs.VFile
	err := fs.Walk(location, func(file vfs.VFile) error {
		pass, filterErr := filter(file)
		if filterErr != nil {
			return filterErr
		}
		if pass {
			files = append(files, file)
		}
		return nil
	})
	return files, err
}

// DeleteMatching deletes files that match the given filter.
func (fs *BlobFS) DeleteMatching(location *url.URL, filter vfs.FileFilter) error {
	files, err := fs.Find(location, filter)
	if err != nil {
		return err
	}
	for _, file := range files {
		if delErr := file.Delete(); delErr != nil {
			return delErr
		}
	}
	return nil
}

// CopyRaw copies from src URL string to dst URL string.
func (fs *BlobFS) CopyRaw(src, dst string) error {
	srcURL, err := url.Parse(src)
	if err != nil {
		return err
	}
	dstURL, err := url.Parse(dst)
	if err != nil {
		return err
	}
	return fs.Copy(srcURL, dstURL)
}

// CreateRaw creates a file at the given URL string.
func (fs *BlobFS) CreateRaw(raw string) (vfs.VFile, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return fs.Create(u)
}

// DeleteRaw deletes the blob at the given URL string.
func (fs *BlobFS) DeleteRaw(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return fs.Delete(u)
}

// ListRaw lists blobs at the given URL string.
func (fs *BlobFS) ListRaw(raw string) ([]vfs.VFile, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return fs.List(u)
}

// MkdirRaw creates a directory at the given URL string.
func (fs *BlobFS) MkdirRaw(raw string) (vfs.VFile, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return fs.Mkdir(u)
}

// MkdirAllRaw creates a directory at the given URL string.
func (fs *BlobFS) MkdirAllRaw(raw string) (vfs.VFile, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return fs.MkdirAll(u)
}

// MoveRaw moves from src URL string to dst URL string.
func (fs *BlobFS) MoveRaw(src, dst string) error {
	srcURL, err := url.Parse(src)
	if err != nil {
		return err
	}
	dstURL, err := url.Parse(dst)
	if err != nil {
		return err
	}
	return fs.Move(srcURL, dstURL)
}

// OpenRaw opens the blob at the given URL string.
func (fs *BlobFS) OpenRaw(raw string) (vfs.VFile, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return fs.Open(u)
}

// WalkRaw walks the blob prefix tree at the given URL string.
func (fs *BlobFS) WalkRaw(raw string, fn vfs.WalkFn) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return fs.Walk(u, fn)
}
