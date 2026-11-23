package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	azblobblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"oss.nandlabs.io/golly/textutils"
	"oss.nandlabs.io/golly/vfs"
)

// Compile-time check that BlobFS satisfies vfs.VFileSystemCtx so callers can
// type-assert (or rely on vfs.OpenCtx/CopyCtx/... to dispatch automatically).
var _ vfs.VFileSystemCtx = (*BlobFS)(nil)

// copyPollInterval is how often a pending server-side copy is polled.
var copyPollInterval = 500 * time.Millisecond

// errStopIteration is an internal sentinel used to end a listing early.
var errStopIteration = errors.New("azblob: stop iteration")

// OpenCtx is the context-aware variant of Open. It does not validate
// existence and makes no remote call.
func (fs *BlobFS) OpenCtx(_ context.Context, u *url.URL) (vfs.VFile, error) {
	opts, err := parseURL(u)
	if err != nil {
		return nil, err
	}
	client, err := getContainerClient(opts)
	if err != nil {
		return nil, err
	}
	return fs.newFile(client, opts), nil
}

// CreateCtx is the context-aware variant of Create.
func (fs *BlobFS) CreateCtx(ctx context.Context, u *url.URL) (vfs.VFile, error) {
	opts, err := parseURL(u)
	if err != nil {
		return nil, err
	}
	if opts.Key == "" {
		return nil, fmt.Errorf("azblob: cannot create a file at the container root %s", u)
	}
	client, err := getContainerClient(opts)
	if err != nil {
		return nil, err
	}
	_, err = client.NewBlockBlobClient(opts.Key).Upload(ctx, streaming.NopCloser(bytes.NewReader(nil)),
		&blockblob.UploadOptions{
			HTTPHeaders: &azblobblob.HTTPHeaders{BlobContentType: to.Ptr(contentTypeFor(opts.Key))},
			AccessConditions: &azblobblob.AccessConditions{
				ModifiedAccessConditions: &azblobblob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
			},
		})
	if err != nil {
		if isAlreadyExists(err) {
			return nil, fmt.Errorf("%w: file %s already exists", iofs.ErrExist, u)
		}
		return nil, mapBlobErr(err)
	}
	return fs.newFile(client, opts), nil
}

// MkdirAllCtx is the context-aware variant of MkdirAll.
func (fs *BlobFS) MkdirAllCtx(ctx context.Context, u *url.URL) (vfs.VFile, error) {
	opts, err := parseURL(u)
	if err != nil {
		return nil, err
	}
	client, err := getContainerClient(opts)
	if err != nil {
		return nil, err
	}

	if opts.Key == "" {
		if err := createContainer(ctx, client); err != nil {
			return nil, err
		}
		return fs.newFile(client, opts), nil
	}

	key := dirPrefix(opts.Key)
	err = putMarker(ctx, client, key)
	if err != nil && bloberror.HasCode(err, bloberror.ContainerNotFound) {
		if cerr := createContainer(ctx, client); cerr != nil {
			return nil, cerr
		}
		err = putMarker(ctx, client, key)
	}
	if err != nil {
		return nil, mapBlobErr(err)
	}
	return fs.newFile(client, opts.withKey(key)), nil
}

// putMarker uploads a zero-byte directory marker blob.
func putMarker(ctx context.Context, cc *container.Client, key string) error {
	_, err := cc.NewBlockBlobClient(key).Upload(ctx, streaming.NopCloser(bytes.NewReader(nil)), nil)
	return err
}

// createContainer creates the container, treating "already exists" as success.
func createContainer(ctx context.Context, cc *container.Client) error {
	_, err := cc.Create(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return mapBlobErr(err)
	}
	return nil
}

// DeleteCtx is the context-aware variant of Delete.
func (fs *BlobFS) DeleteCtx(ctx context.Context, src *url.URL) error {
	opts, err := parseURL(src)
	if err != nil {
		return err
	}
	client, err := getContainerClient(opts)
	if err != nil {
		return err
	}
	f := fs.newFile(client, opts)
	info, err := f.infoCtx(ctx)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return f.deleteAllCtx(ctx)
	}
	return f.deleteCtx(ctx)
}

// ListCtx is the context-aware variant of List. It returns the direct
// children of the prefix: blobs, and virtual directories (whose keys end
// in "/").
func (fs *BlobFS) ListCtx(ctx context.Context, u *url.URL) ([]vfs.VFile, error) {
	it, err := fs.ListIter(ctx, u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = it.Close() }()
	var files []vfs.VFile
	for {
		f, err := it.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return files, nil
			}
			return nil, err
		}
		files = append(files, f)
	}
}

// WalkCtx is the context-aware variant of Walk. ctx is checked between
// pages and before each callback.
func (fs *BlobFS) WalkCtx(ctx context.Context, u *url.URL, fn vfs.WalkFn) error {
	opts, err := parseURL(u)
	if err != nil {
		return err
	}
	client, err := getContainerClient(opts)
	if err != nil {
		return err
	}
	prefix := dirPrefix(opts.Key)
	var skipped []string

	err = forEachBlob(ctx, client, prefix, func(item *container.BlobItem) error {
		key := *item.Name
		if key == prefix {
			return nil
		}
		for _, sp := range skipped {
			if strings.HasPrefix(key, sp) {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		walkErr := fn(fs.newFile(client, opts.withKey(key)))
		switch {
		case walkErr == nil:
			return nil
		case errors.Is(walkErr, vfs.ErrSkipAll):
			return errStopIteration
		case errors.Is(walkErr, vfs.ErrSkipDir):
			skip := key
			if !isDirKey(key) {
				// SkipDir on a file skips the rest of its directory.
				skip = key[:strings.LastIndex(key, textutils.ForwardSlashStr)+1]
			}
			if skip == prefix {
				return errStopIteration
			}
			skipped = append(skipped, skip)
			return nil
		default:
			return walkErr
		}
	})
	if errors.Is(err, errStopIteration) {
		return nil
	}
	return err
}

// CopyCtx is the context-aware variant of Copy. For directories every blob
// under the source prefix (including directory markers) is copied to the
// same relative path under dst.
func (fs *BlobFS) CopyCtx(ctx context.Context, src, dst *url.URL) error {
	srcOpts, err := parseURL(src)
	if err != nil {
		return err
	}
	dstOpts, err := parseURL(dst)
	if err != nil {
		return err
	}
	srcClient, err := getContainerClient(srcOpts)
	if err != nil {
		return err
	}
	dstClient, err := getContainerClient(dstOpts)
	if err != nil {
		return err
	}

	srcInfo, err := fs.newFile(srcClient, srcOpts).infoCtx(ctx)
	if err != nil {
		return err
	}
	if !srcInfo.IsDir() {
		return copyBlob(ctx, srcClient, srcOpts.Key, dstClient, dstOpts.Key)
	}

	srcPrefix := dirPrefix(srcOpts.Key)
	dstPrefix := dirPrefix(dstOpts.Key)
	return forEachBlob(ctx, srcClient, srcPrefix, func(item *container.BlobItem) error {
		key := *item.Name
		dstKey := dstPrefix + strings.TrimPrefix(key, srcPrefix)
		if dstKey == "" {
			return nil
		}
		return copyBlob(ctx, srcClient, key, dstClient, dstKey)
	})
}

// copyBlob copies a single blob. It first tries a server-side Copy Blob
// (waiting for completion when the service reports it pending) and falls
// back to a streamed download + upload when the server-side copy is
// rejected (typically cross-account without a SAS on the source).
func copyBlob(ctx context.Context, srcCC *container.Client, srcKey string, dstCC *container.Client, dstKey string) error {
	srcBC := srcCC.NewBlobClient(srcKey)
	dstBC := dstCC.NewBlobClient(dstKey)

	resp, err := dstBC.StartCopyFromURL(ctx, srcBC.URL(), nil)
	if err == nil {
		if resp.CopyStatus != nil && *resp.CopyStatus == azblobblob.CopyStatusTypePending {
			return waitForCopy(ctx, dstBC)
		}
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	logger.DebugF("azblob: server-side copy %s -> %s failed (%v); falling back to stream copy", srcBC.URL(), dstBC.URL(), err)
	return streamCopy(ctx, srcBC, dstCC.NewBlockBlobClient(dstKey))
}

// waitForCopy polls the destination blob until a pending copy finishes.
func waitForCopy(ctx context.Context, dst *azblobblob.Client) error {
	for {
		props, err := dst.GetProperties(ctx, nil)
		if err != nil {
			return mapBlobErr(err)
		}
		status := azblobblob.CopyStatusTypeSuccess
		if props.CopyStatus != nil {
			status = *props.CopyStatus
		}
		switch status {
		case azblobblob.CopyStatusTypeSuccess:
			return nil
		case azblobblob.CopyStatusTypePending:
		default:
			desc := ""
			if props.CopyStatusDescription != nil {
				desc = *props.CopyStatusDescription
			}
			return fmt.Errorf("azblob: copy to %s %s: %s", dst.URL(), status, desc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(copyPollInterval):
		}
	}
}

// streamCopy downloads src and uploads it to dst, preserving content type
// and metadata.
func streamCopy(ctx context.Context, src *azblobblob.Client, dst *blockblob.Client) error {
	resp, err := src.DownloadStream(ctx, nil)
	if err != nil {
		return mapBlobErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	opts := &blockblob.UploadStreamOptions{Metadata: resp.Metadata}
	if resp.ContentType != nil {
		opts.HTTPHeaders = &azblobblob.HTTPHeaders{BlobContentType: resp.ContentType}
	}
	_, err = dst.UploadStream(ctx, resp.Body, opts)
	return mapBlobErr(err)
}

// MoveCtx is CopyCtx + DeleteCtx. Moving a URL onto itself is a no-op.
func (fs *BlobFS) MoveCtx(ctx context.Context, src, dst *url.URL) error {
	if src != nil && dst != nil && src.String() == dst.String() {
		if _, err := parseURL(src); err != nil {
			return err
		}
		return nil
	}
	if err := fs.CopyCtx(ctx, src, dst); err != nil {
		return err
	}
	return fs.DeleteCtx(ctx, src)
}
