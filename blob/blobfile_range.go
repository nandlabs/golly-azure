package blob

import (
	"context"
	"fmt"
	"io"

	azblobblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"oss.nandlabs.io/golly/vfs"
)

// Compile-time check that BlobFile implements the optional vfs.RangeReader
// capability so vfs.ReadRange dispatches through us — translating the
// caller's [off, off+length) to a ranged Get Blob request rather than
// falling back to Seek+Read.
var _ vfs.RangeReader = (*BlobFile)(nil)

// ReadRange returns up to length bytes starting at off via a ranged Get
// Blob call. A length of 0 means "read to EOF from off". Returns
// (nil, io.EOF) when off is at or past the end of the blob.
//
// Only the requested bytes are transferred over the wire, no matter how
// large the underlying blob is.
func (f *BlobFile) ReadRange(ctx context.Context, off, length int64) ([]byte, error) {
	if off < 0 {
		return nil, fmt.Errorf("azblob: negative offset %d", off)
	}
	if length < 0 {
		return nil, fmt.Errorf("azblob: negative length %d", length)
	}

	resp, err := f.blobClient().DownloadStream(ctx, &azblobblob.DownloadStreamOptions{
		Range: azblobblob.HTTPRange{Offset: off, Count: length},
	})
	if err != nil {
		// Azure answers 416 InvalidRange when off >= blob size.
		if isInvalidRange(err) {
			return nil, io.EOF
		}
		return nil, mapBlobErr(err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if length > 0 && int64(len(buf)) > length {
		buf = buf[:length]
	}
	if len(buf) == 0 && (length > 0 || off > 0) {
		return nil, io.EOF
	}
	return buf, nil
}
