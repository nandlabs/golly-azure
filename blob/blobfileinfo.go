package blob

import (
	"fmt"
	"os"
	"time"

	"oss.nandlabs.io/golly/vfs"
)

// BlobFileInfo implements the vfs.VFileInfo interface for Azure blobs.
type BlobFileInfo struct {
	fs           vfs.VFileSystem
	isDir        bool
	key          string
	lastModified time.Time
	size         int64
	contentType  string
	etag         string
}

// Name returns the blob name (key within the container).
func (f *BlobFileInfo) Name() string {
	return f.key
}

// Size returns the size of the blob in bytes.
func (f *BlobFileInfo) Size() int64 {
	return f.size
}

// Mode returns the file mode bits. Not applicable for Azure blobs, returns 0
// (or os.ModeDir for directories).
func (f *BlobFileInfo) Mode() os.FileMode {
	if f.isDir {
		return os.ModeDir
	}
	return 0
}

// ModTime returns the last modified time of the blob.
func (f *BlobFileInfo) ModTime() time.Time {
	return f.lastModified
}

// IsDir returns true if the entry represents a directory (virtual prefix,
// trailing-slash marker blob, or container root).
func (f *BlobFileInfo) IsDir() bool {
	return f.isDir
}

// Sys returns the underlying VFileSystem.
func (f *BlobFileInfo) Sys() interface{} {
	return f.fs
}

// ContentType returns the blob's Content-Type ("" for directories).
func (f *BlobFileInfo) ContentType() string {
	return f.contentType
}

// ETag returns the blob's ETag ("" for virtual directories).
func (f *BlobFileInfo) ETag() string {
	return f.etag
}

// String returns a string representation of the file info.
func (f *BlobFileInfo) String() string {
	return fmt.Sprintf("BlobFileInfo{Name: %s, Size: %d, ModTime: %v, IsDir: %t}", f.key, f.size, f.lastModified, f.isDir)
}
