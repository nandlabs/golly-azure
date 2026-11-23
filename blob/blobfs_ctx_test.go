package blob

import (
	"testing"

	"oss.nandlabs.io/golly/vfs"
)

// TestBlobFS_ImplementsVFileSystemCtx guarantees the package-level
// dispatchers in golly (vfs.OpenCtx, vfs.CopyCtx, …) route through our
// ctx-aware methods rather than the goroutine fallback.
func TestBlobFS_ImplementsVFileSystemCtx(t *testing.T) {
	var _ vfs.VFileSystemCtx = (*BlobFS)(nil)
}

// TestBlobFS_ImplementsLister guarantees vfs.ListIter dispatches through
// BlobFS.ListIter rather than the eager List slice.
func TestBlobFS_ImplementsLister(t *testing.T) {
	var _ vfs.Lister = (*BlobFS)(nil)
}

// TestBlobFile_ImplementsRangeReader guarantees vfs.ReadRange dispatches
// to a native ranged Get Blob instead of Seek+Read.
func TestBlobFile_ImplementsRangeReader(t *testing.T) {
	var _ vfs.RangeReader = (*BlobFile)(nil)
}
