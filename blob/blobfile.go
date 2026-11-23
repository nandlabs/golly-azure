package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	azblobblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"oss.nandlabs.io/golly/textutils"
	"oss.nandlabs.io/golly/vfs"
)

// defaultContentType is used when no content type is known for a blob.
const defaultContentType = "application/octet-stream"

// BlobFile implements the vfs.VFile interface for Azure blobs.
//
// Reads stream from the blob lazily (starting at the current offset, so
// Seek is supported for reads); writes are buffered in memory and
// uploaded as a block blob on Close.
type BlobFile struct {
	*vfs.BaseFile
	client  *container.Client
	fs      *BlobFS
	urlOpts *urlOpts
	// reader/writer state
	reader      io.ReadCloser
	writeBuffer *bytes.Buffer
	offset      int64
	contentType string
}

// newBlobFile creates a new BlobFile instance.
func newBlobFile(client *container.Client, fs *BlobFS, opts *urlOpts) *BlobFile {
	f := &BlobFile{
		client:  client,
		fs:      fs,
		urlOpts: opts,
	}
	f.BaseFile = &vfs.BaseFile{VFile: f}
	return f
}

// blobClient returns the SDK blob client for this file.
func (f *BlobFile) blobClient() *azblobblob.Client {
	return f.client.NewBlobClient(f.urlOpts.Key)
}

// Read reads from the blob, starting at the current offset.
func (f *BlobFile) Read(b []byte) (n int, err error) {
	if f.reader == nil {
		resp, getErr := f.blobClient().DownloadStream(context.Background(), &azblobblob.DownloadStreamOptions{
			Range: azblobblob.HTTPRange{Offset: f.offset},
		})
		if getErr != nil {
			if f.offset > 0 && isInvalidRange(getErr) {
				return 0, io.EOF
			}
			return 0, mapBlobErr(getErr)
		}
		f.reader = resp.Body
		if resp.ContentType != nil {
			f.contentType = *resp.ContentType
		}
	}
	n, err = f.reader.Read(b)
	f.offset += int64(n)
	return
}

// Write writes data to an in-memory buffer. The data is uploaded to Azure on Close.
func (f *BlobFile) Write(b []byte) (n int, err error) {
	if f.writeBuffer == nil {
		f.writeBuffer = &bytes.Buffer{}
	}
	n, err = f.writeBuffer.Write(b)
	f.offset += int64(n)
	return
}

// WriteString writes s to the in-memory buffer. It is defined here rather
// than inherited from vfs.BaseFile because BaseFile.WriteString (golly
// v1.8.1) calls io.WriteString on itself and recurses forever.
func (f *BlobFile) WriteString(s string) (int, error) {
	return f.Write([]byte(s))
}

// Seek sets the offset for the next Read. All whence values are supported
// for reads (io.SeekEnd costs one GetProperties call); the next Read
// issues a ranged download from the new offset. While a write is
// buffered only a no-op Seek(0, io.SeekCurrent) is allowed.
func (f *BlobFile) Seek(offset int64, whence int) (int64, error) {
	if f.writeBuffer != nil {
		if offset == 0 && whence == io.SeekCurrent {
			return f.offset, nil
		}
		return f.offset, fmt.Errorf("azblob: seek during buffered write: %w", vfs.ErrNotSupported)
	}
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = f.offset + offset
	case io.SeekEnd:
		props, err := f.blobClient().GetProperties(context.Background(), nil)
		if err != nil {
			return f.offset, mapBlobErr(err)
		}
		size := int64(0)
		if props.ContentLength != nil {
			size = *props.ContentLength
		}
		target = size + offset
	default:
		return f.offset, fmt.Errorf("azblob: invalid whence %d", whence)
	}
	if target < 0 {
		return f.offset, fmt.Errorf("azblob: negative seek position %d", target)
	}
	if target != f.offset && f.reader != nil {
		_ = f.reader.Close()
		f.reader = nil
	}
	f.offset = target
	return target, nil
}

// Close uploads any buffered writes to Azure and closes open readers.
func (f *BlobFile) Close() error {
	var err error
	if f.writeBuffer != nil {
		ct := f.contentType
		if ct == "" {
			ct = contentTypeFor(f.urlOpts.Key)
		}
		_, err = f.client.NewBlockBlobClient(f.urlOpts.Key).UploadBuffer(context.Background(), f.writeBuffer.Bytes(),
			&blockblob.UploadBufferOptions{HTTPHeaders: &azblobblob.HTTPHeaders{BlobContentType: to.Ptr(ct)}})
		err = mapBlobErr(err)
		if err == nil {
			f.contentType = ct
		}
		f.writeBuffer = nil
	}
	if f.reader != nil {
		closeErr := f.reader.Close()
		if err == nil {
			err = closeErr
		}
		f.reader = nil
	}
	return err
}

// ListAll lists all blobs (recursively) under this prefix.
func (f *BlobFile) ListAll() ([]vfs.VFile, error) {
	return f.listAllCtx(context.Background())
}

func (f *BlobFile) listAllCtx(ctx context.Context) (files []vfs.VFile, err error) {
	prefix := dirPrefix(f.urlOpts.Key)
	err = forEachBlob(ctx, f.client, prefix, func(item *container.BlobItem) error {
		key := *item.Name
		if key == prefix || key == f.urlOpts.Key {
			return nil
		}
		files = append(files, f.fs.newFile(f.client, f.urlOpts.withKey(key)))
		return nil
	})
	return
}

// Delete deletes the blob (including its snapshots).
func (f *BlobFile) Delete() error {
	return f.deleteCtx(context.Background())
}

func (f *BlobFile) deleteCtx(ctx context.Context) error {
	return deleteBlob(ctx, f.client, f.urlOpts.Key)
}

// DeleteAll deletes every blob under this prefix, the directory marker
// blob (if any) and — when this file is not a directory key — the blob
// itself. Missing blobs are ignored.
func (f *BlobFile) DeleteAll() error {
	return f.deleteAllCtx(context.Background())
}

func (f *BlobFile) deleteAllCtx(ctx context.Context) error {
	prefix := dirPrefix(f.urlOpts.Key)
	var keys []string
	if err := forEachBlob(ctx, f.client, prefix, func(item *container.BlobItem) error {
		keys = append(keys, *item.Name)
		return nil
	}); err != nil {
		return err
	}
	if prefix != "" {
		keys = append(keys, prefix)
	}
	if !isDirKey(f.urlOpts.Key) {
		keys = append(keys, f.urlOpts.Key)
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := deleteBlob(ctx, f.client, key); err != nil && !errors.Is(err, vfs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Info returns the VFileInfo for this blob. Keys ending in "/" (and the
// container root) are reported as directories without a remote call; a
// missing blob that has children under "<key>/" is reported as a
// virtual directory.
func (f *BlobFile) Info() (vfs.VFileInfo, error) {
	return f.infoCtx(context.Background())
}

func (f *BlobFile) infoCtx(ctx context.Context) (*BlobFileInfo, error) {
	if isDirKey(f.urlOpts.Key) {
		return &BlobFileInfo{fs: f.fs, isDir: true, key: f.urlOpts.Key}, nil
	}

	props, err := f.blobClient().GetProperties(ctx, nil)
	if err != nil {
		if !isNotFound(err) {
			return nil, mapBlobErr(err)
		}
		// No blob at key — it may be a virtual directory.
		isDir, listErr := hasChildren(ctx, f.client, dirPrefix(f.urlOpts.Key))
		if listErr != nil {
			return nil, mapBlobErr(listErr)
		}
		if isDir {
			return &BlobFileInfo{fs: f.fs, isDir: true, key: f.urlOpts.Key}, nil
		}
		return nil, mapBlobErr(err)
	}

	info := &BlobFileInfo{fs: f.fs, key: f.urlOpts.Key}
	if props.ContentLength != nil {
		info.size = *props.ContentLength
	}
	if props.LastModified != nil {
		info.lastModified = *props.LastModified
	}
	if props.ContentType != nil {
		info.contentType = *props.ContentType
		f.contentType = *props.ContentType
	}
	if props.ETag != nil {
		info.etag = string(*props.ETag)
	}
	return info, nil
}

// Parent returns the parent directory of this file. The parent of a
// top-level blob is the container root.
func (f *BlobFile) Parent() (vfs.VFile, error) {
	key := strings.TrimSuffix(f.urlOpts.Key, textutils.ForwardSlashStr)
	idx := strings.LastIndex(key, textutils.ForwardSlashStr)
	parentKey := ""
	if idx > 0 {
		parentKey = key[:idx+1]
	}
	return f.fs.newFile(f.client, f.urlOpts.withKey(parentKey)), nil
}

// Url returns the URL of this file.
func (f *BlobFile) Url() *url.URL {
	return f.urlOpts.u
}

// ContentType returns the content type of the blob, as observed by the
// last Read, Info or Close; it falls back to application/octet-stream.
func (f *BlobFile) ContentType() string {
	if f.contentType != "" {
		return f.contentType
	}
	return defaultContentType
}

// AddProperty adds (or replaces) a user-defined metadata entry on the
// blob. Azure metadata names are case-insensitive and must be valid C#
// identifiers. The update is guarded by If-Match on the ETag read, so
// concurrent metadata writers do not silently clobber each other.
func (f *BlobFile) AddProperty(name, value string) error {
	ctx := context.Background()
	bc := f.blobClient()

	props, err := bc.GetProperties(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to get blob metadata: %w", mapBlobErr(err))
	}

	metadata := make(map[string]*string, len(props.Metadata)+1)
	for k, v := range props.Metadata {
		if !strings.EqualFold(k, name) {
			metadata[k] = v
		}
	}
	metadata[name] = to.Ptr(value)

	var setOpts *azblobblob.SetMetadataOptions
	if props.ETag != nil {
		setOpts = &azblobblob.SetMetadataOptions{AccessConditions: &azblobblob.AccessConditions{
			ModifiedAccessConditions: &azblobblob.ModifiedAccessConditions{IfMatch: props.ETag},
		}}
	}
	if _, err = bc.SetMetadata(ctx, metadata, setOpts); err != nil {
		return fmt.Errorf("failed to update blob metadata: %w", mapBlobErr(err))
	}

	logger.InfoF("Added metadata %q=%q to %s", name, value, f.urlOpts.u.String())
	return nil
}

// GetProperty retrieves a user-defined metadata value from the blob.
// The lookup is case-insensitive, matching Azure's metadata semantics.
func (f *BlobFile) GetProperty(name string) (string, error) {
	props, err := f.blobClient().GetProperties(context.Background(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to get blob metadata: %w", mapBlobErr(err))
	}
	for k, v := range props.Metadata {
		if strings.EqualFold(k, name) && v != nil {
			return *v, nil
		}
	}
	return "", fmt.Errorf("metadata key %q not found", name)
}

// String returns the URL string of the file.
func (f *BlobFile) String() string {
	return f.urlOpts.u.String()
}

// contentTypeFor guesses a content type from the blob name's extension.
func contentTypeFor(key string) string {
	if ct := mime.TypeByExtension(path.Ext(key)); ct != "" {
		return ct
	}
	return defaultContentType
}

// deleteBlob deletes a single blob together with its snapshots.
func deleteBlob(ctx context.Context, cc *container.Client, key string) error {
	_, err := cc.NewBlobClient(key).Delete(ctx, &azblobblob.DeleteOptions{
		DeleteSnapshots: to.Ptr(azblobblob.DeleteSnapshotsOptionTypeInclude),
	})
	return mapBlobErr(err)
}

// forEachBlob calls fn for every blob whose name starts with prefix
// (flat listing, all pages).
func forEachBlob(ctx context.Context, cc *container.Client, prefix string, fn func(*container.BlobItem) error) error {
	var p *string
	if prefix != "" {
		p = to.Ptr(prefix)
	}
	pager := cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: p})
	for pager.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			return mapBlobErr(err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			if err := fn(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasChildren reports whether at least one blob exists under prefix.
func hasChildren(ctx context.Context, cc *container.Client, prefix string) (bool, error) {
	pager := cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     to.Ptr(prefix),
		MaxResults: to.Ptr(int32(1)),
	})
	if !pager.More() {
		return false, nil
	}
	page, err := pager.NextPage(ctx)
	if err != nil {
		return false, err
	}
	return page.Segment != nil && len(page.Segment.BlobItems) > 0, nil
}
