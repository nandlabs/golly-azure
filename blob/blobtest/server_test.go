package blobtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

func newClient(t *testing.T, s *Server) *azblob.Client {
	t.Helper()
	c, err := azblob.NewClientFromConnectionString(s.ConnectionString(), &azblob.ClientOptions{
		ClientOptions: policy.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func TestContainerLifecycle(t *testing.T) {
	s := NewServer()
	defer s.Close()
	c := newClient(t, s)
	ctx := context.Background()

	if _, err := c.CreateContainer(ctx, "c1", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.CreateContainer(ctx, "c1", nil); !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		t.Fatalf("dup create err = %v", err)
	}
	cc := c.ServiceClient().NewContainerClient("c1")
	if _, err := cc.GetProperties(ctx, nil); err != nil {
		t.Fatalf("props: %v", err)
	}
	if _, err := c.DeleteContainer(ctx, "c1", nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.DeleteContainer(ctx, "c1", nil); !bloberror.HasCode(err, bloberror.ContainerNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	if _, err := cc.GetProperties(ctx, nil); !bloberror.HasCode(err, bloberror.ContainerNotFound) {
		t.Fatalf("props missing err = %v", err)
	}
	pager := cc.NewListBlobsFlatPager(nil)
	if _, err := pager.NextPage(ctx); !bloberror.HasCode(err, bloberror.ContainerNotFound) {
		t.Fatalf("list missing err = %v", err)
	}
	if s.HasContainer("c1") {
		t.Fatal("container should be gone")
	}
}

func TestBlockListUploadAndDownload(t *testing.T) {
	s := NewServer()
	defer s.Close()
	s.CreateContainer("c")
	c := newClient(t, s)
	ctx := context.Background()

	// UploadStream stages blocks + commits a block list.
	data := bytes.Repeat([]byte("0123456789"), 300_000) // 3 MB, > 1 MiB block size
	bb := c.ServiceClient().NewContainerClient("c").NewBlockBlobClient("big.bin")
	if _, err := bb.UploadStream(ctx, bytes.NewReader(data), &blockblob.UploadStreamOptions{
		BlockSize:   1 << 20,
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: to.Ptr("application/x-test")},
		Metadata:    map[string]*string{"k": to.Ptr("v")},
	}); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
	got, ok := s.GetBlob("c", "big.bin")
	if !ok || !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes (ok=%v), want %d", len(got), ok, len(data))
	}
	if s.BlobContentType("c", "big.bin") != "application/x-test" || s.BlobMetadata("c", "big.bin")["k"] != "v" {
		t.Fatal("headers / metadata not stored")
	}

	resp, err := bb.DownloadStream(ctx, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: 10, Count: 5}})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "01234" {
		t.Fatalf("range body = %q", body)
	}
	if _, err := bb.DownloadStream(ctx, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: int64(len(data))}}); !bloberror.HasCode(err, bloberror.InvalidRange) {
		t.Fatalf("out-of-range err = %v", err)
	}
}

func TestConditionalWrites(t *testing.T) {
	s := NewServer()
	defer s.Close()
	s.CreateContainer("c")
	bb := newClient(t, s).ServiceClient().NewContainerClient("c").NewBlockBlobClient("lock")
	ctx := context.Background()
	body := func(v string) io.ReadSeekCloser { return streaming.NopCloser(strings.NewReader(v)) }
	ifNone := &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)}}
	ifMatch := func(e azcore.ETag) *blob.AccessConditions {
		return &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(e)}}
	}

	r1, err := bb.Upload(ctx, body("a"), &blockblob.UploadOptions{AccessConditions: ifNone})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := bb.Upload(ctx, body("b"), &blockblob.UploadOptions{AccessConditions: ifNone}); !bloberror.HasCode(err, bloberror.BlobAlreadyExists) {
		t.Fatalf("second create err = %v", err)
	}
	r2, err := bb.Upload(ctx, body("c"), &blockblob.UploadOptions{AccessConditions: ifMatch(*r1.ETag)})
	if err != nil {
		t.Fatalf("if-match write: %v", err)
	}
	if _, err := bb.Upload(ctx, body("d"), &blockblob.UploadOptions{AccessConditions: ifMatch(*r1.ETag)}); !bloberror.HasCode(err, bloberror.ConditionNotMet) {
		t.Fatalf("stale if-match err = %v", err)
	}
	if _, err := bb.Delete(ctx, &blob.DeleteOptions{AccessConditions: ifMatch(*r1.ETag)}); !bloberror.HasCode(err, bloberror.ConditionNotMet) {
		t.Fatalf("stale delete err = %v", err)
	}
	if _, err := bb.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: ifMatch(*r1.ETag)}); err == nil {
		t.Fatal("stale conditional HEAD should fail")
	}
	if _, err := bb.Delete(ctx, &blob.DeleteOptions{AccessConditions: ifMatch(*r2.ETag)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := bb.Delete(ctx, &blob.DeleteOptions{AccessConditions: ifMatch(*r2.ETag)}); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	if _, err := bb.Upload(ctx, body("e"), &blockblob.UploadOptions{AccessConditions: ifMatch(*r2.ETag)}); !bloberror.HasCode(err, bloberror.ConditionNotMet) {
		t.Fatalf("if-match on missing err = %v", err)
	}
	if _, err := bb.SetMetadata(ctx, map[string]*string{"a": to.Ptr("b")}, nil); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("metadata on missing err = %v", err)
	}
}

func TestCopyAndInjection(t *testing.T) {
	s := NewServer()
	defer s.Close()
	s.PutBlob("c", "src", []byte("x"), "text/plain")
	cc := newClient(t, s).ServiceClient().NewContainerClient("c")
	ctx := context.Background()
	src := cc.NewBlobClient("src").URL()

	if _, err := cc.NewBlobClient("dst").StartCopyFromURL(ctx, src, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if d, _ := s.GetBlob("c", "dst"); string(d) != "x" {
		t.Fatalf("copy content = %q", d)
	}
	if _, err := cc.NewBlobClient("dst2").StartCopyFromURL(ctx, cc.NewBlobClient("missing").URL(), nil); !bloberror.HasCode(err, bloberror.CannotVerifyCopySource) {
		t.Fatalf("copy missing err = %v", err)
	}
	s.RejectCopy(true)
	if _, err := cc.NewBlobClient("dst3").StartCopyFromURL(ctx, src, nil); !bloberror.HasCode(err, bloberror.CannotVerifyCopySource) {
		t.Fatalf("rejected copy err = %v", err)
	}
	s.RejectCopy(false)

	s.InjectError(http.MethodGet, http.StatusInternalServerError, "InternalError")
	_, err := cc.NewBlobClient("src").DownloadStream(ctx, nil)
	var re *azcore.ResponseError
	if !errors.As(err, &re) || re.StatusCode != 500 || re.ErrorCode != "InternalError" {
		t.Fatalf("injected err = %v", err)
	}
	if _, err := cc.NewBlobClient("src").DownloadStream(ctx, nil); err != nil {
		t.Fatalf("injection should be one-shot: %v", err)
	}
	if s.RequestCount(http.MethodGet) < 2 {
		t.Fatal("request counter not incremented")
	}
	if names := s.BlobNames("c"); len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
	if s.BlobNames("nope") != nil || s.BlobContentType("c", "nope") != "" {
		t.Fatal("missing lookups should be empty")
	}
	if _, ok := s.GetBlob("nope", "x"); ok {
		t.Fatal("missing container GetBlob")
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		in         string
		size       int64
		start, end int64
		ok         bool
	}{
		{"bytes=0-4", 10, 0, 4, true},
		{"bytes=5-", 10, 5, 9, true},
		{"bytes=8-100", 10, 8, 9, true},
		{"bytes=10-", 10, 0, 0, false},
		{"bytes=x-", 10, 0, 0, false},
		{"bytes=5-2", 10, 0, 0, false},
		{"items=0-1", 10, 0, 0, false},
	}
	for _, c := range cases {
		s, e, ok := parseRange(c.in, c.size)
		if ok != c.ok || (ok && (s != c.start || e != c.end)) {
			t.Errorf("parseRange(%q,%d) = %d,%d,%v", c.in, c.size, s, e, ok)
		}
	}
}

func TestUnsupportedRequests(t *testing.T) {
	s := NewServer()
	defer s.Close()
	s.CreateContainer("c")
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/" + AccountName, http.StatusBadRequest},
		{http.MethodPost, "/" + AccountName + "/c?restype=container", http.StatusMethodNotAllowed},
		{http.MethodPost, "/" + AccountName + "/c/b", http.StatusMethodNotAllowed},
		{http.MethodPut, "/" + AccountName + "/c/b?comp=weird", http.StatusBadRequest},
		{http.MethodPut, "/" + AccountName + "/c/b?comp=blocklist", http.StatusBadRequest},
	} {
		req, _ := http.NewRequest(tc.method, s.URL+tc.path, strings.NewReader("<bad"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
	// Block list referencing an unknown block.
	req, _ := http.NewRequest(http.MethodPut, s.URL+"/"+AccountName+"/c/b?comp=blocklist",
		strings.NewReader("<BlockList><Latest>nope</Latest></BlockList>"))
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown block = %d", resp.StatusCode)
	}
}
