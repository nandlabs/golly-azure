package blob

import (
	"context"
	"io"
	"net/url"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"oss.nandlabs.io/golly/textutils"
	"oss.nandlabs.io/golly/vfs"
)

// Compile-time check that BlobFS implements the optional Lister capability
// so vfs.ListIter dispatches through us rather than falling back to the
// eager List slice.
var _ vfs.Lister = (*BlobFS)(nil)

// ListIter returns a paginated FileIterator over the direct children of
// the prefix at u (hierarchical listing with "/" delimiter). Each Next()
// call advances within (or across) List Blobs pages; the next page is
// fetched lazily, so a million-blob prefix never lands in a single slice.
func (fs *BlobFS) ListIter(_ context.Context, u *url.URL) (vfs.FileIterator, error) {
	opts, err := parseURL(u)
	if err != nil {
		return nil, err
	}
	client, err := getContainerClient(opts)
	if err != nil {
		return nil, err
	}
	prefix := dirPrefix(opts.Key)
	listOpts := &container.ListBlobsHierarchyOptions{}
	if prefix != "" {
		listOpts.Prefix = to.Ptr(prefix)
	}
	return &blobFileIterator{
		fs:     fs,
		opts:   opts,
		prefix: prefix,
		client: client,
		pager:  client.NewListBlobsHierarchyPager(textutils.ForwardSlashStr, listOpts),
	}, nil
}

// blobFileIterator yields VFiles one at a time from List Blobs pages.
// Not safe for concurrent use.
type blobFileIterator struct {
	fs     *BlobFS
	opts   *urlOpts
	prefix string
	client *container.Client
	pager  *runtime.Pager[container.ListBlobsHierarchyResponse]

	// Buffer of pending VFiles from the current page (blobs + prefixes).
	buf  []vfs.VFile
	done bool
}

// Next returns the next VFile under the prefix, fetching the next page
// lazily when the current buffer is drained. Returns io.EOF when exhausted.
func (it *blobFileIterator) Next(ctx context.Context) (vfs.VFile, error) {
	if it.done {
		return nil, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for len(it.buf) == 0 {
		if !it.pager.More() {
			it.done = true
			return nil, io.EOF
		}
		page, err := it.pager.NextPage(ctx)
		if err != nil {
			return nil, mapBlobErr(err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil || *item.Name == it.prefix {
				continue
			}
			it.buf = append(it.buf, it.fs.newFile(it.client, it.opts.withKey(*item.Name)))
		}
		for _, p := range page.Segment.BlobPrefixes {
			if p == nil || p.Name == nil {
				continue
			}
			it.buf = append(it.buf, it.fs.newFile(it.client, it.opts.withKey(*p.Name)))
		}
	}
	f := it.buf[0]
	it.buf = it.buf[1:]
	return f, nil
}

// Close discards any remaining buffered entries and stops further
// pagination. Idempotent; safe to call without consuming to EOF.
func (it *blobFileIterator) Close() error {
	it.done = true
	it.buf = nil
	return nil
}
