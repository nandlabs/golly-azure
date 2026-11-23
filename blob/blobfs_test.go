package blob

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"oss.nandlabs.io/golly-azure/blob/blobtest"
	"oss.nandlabs.io/golly/vfs"
)

func TestManagerRegistration(t *testing.T) {
	if !vfs.GetManager().IsSupported(BlobScheme) {
		t.Fatal("azblob scheme not registered with vfs manager")
	}
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "hello.txt", []byte("hi"), "text/plain")

	f, err := vfs.GetManager().OpenRaw(testURL("hello.txt"))
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	defer func() { _ = f.Close() }()
	if got := readAll(t, f); got != "hi" {
		t.Fatalf("content = %q, want hi", got)
	}
}

func TestCreateWriteRead(t *testing.T) {
	srv := newTestServer(t)
	fs := newFS()

	f, err := fs.CreateRaw(testURL("dir/hello.txt"))
	if err != nil {
		t.Fatalf("CreateRaw: %v", err)
	}
	if _, err := f.WriteString("Hello, Azure!"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, ok := srv.GetBlob(testContainer, "dir/hello.txt")
	if !ok || string(data) != "Hello, Azure!" {
		t.Fatalf("stored = %q (ok=%v)", data, ok)
	}
	if ct := srv.BlobContentType(testContainer, "dir/hello.txt"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type = %q, want text/plain*", ct)
	}
	if ct := f.ContentType(); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("file ContentType = %q", ct)
	}

	r, err := fs.OpenRaw(testURL("dir/hello.txt"))
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	s, err := vfs.ReadAllString(r)
	if err != nil {
		t.Fatalf("AsString: %v", err)
	}
	if s != "Hello, Azure!" {
		t.Fatalf("AsString = %q", s)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}
	if r.(*BlobFile).String() != testURL("dir/hello.txt") {
		t.Fatalf("String() = %q", r.(*BlobFile).String())
	}
}

func TestCreate_AlreadyExists(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "a.txt", []byte("x"), "text/plain")
	_, err := newFS().CreateRaw(testURL("a.txt"))
	if !errors.Is(err, iofs.ErrExist) {
		t.Fatalf("Create on existing blob: err = %v, want fs.ErrExist", err)
	}
	if data, _ := srv.GetBlob(testContainer, "a.txt"); string(data) != "x" {
		t.Fatalf("existing blob clobbered: %q", data)
	}
}

func TestCreate_Errors(t *testing.T) {
	newTestServer(t)
	fs := newFS()
	if _, err := fs.CreateRaw("azblob://" + blobtest.AccountName + "/" + testContainer); err == nil {
		t.Fatal("expected error creating at container root")
	}
	if _, err := fs.CreateRaw("azblob://" + blobtest.AccountName + "/missing/x.txt"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("missing container: err = %v, want ErrNotExist", err)
	}
	if _, err := fs.CreateRaw("s3://bucket/x"); err == nil {
		t.Fatal("expected scheme error")
	}
	if _, err := fs.CreateRaw("://bad"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestOverwriteViaOpen(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "o.bin", []byte("old"), "")
	f, _ := newFS().OpenRaw(testURL("o.bin"))
	if _, err := f.Write([]byte("new-content")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if data, _ := srv.GetBlob(testContainer, "o.bin"); string(data) != "new-content" {
		t.Fatalf("content = %q", data)
	}
	if ct := srv.BlobContentType(testContainer, "o.bin"); ct != defaultContentType {
		t.Fatalf("content type = %q, want %q", ct, defaultContentType)
	}
}

func TestRead_Missing(t *testing.T) {
	newTestServer(t)
	f, err := newFS().OpenRaw(testURL("nope.txt"))
	if err != nil {
		t.Fatalf("Open should not validate existence: %v", err)
	}
	_, err = f.Read(make([]byte, 10))
	if !errors.Is(err, vfs.ErrNotExist) || !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("Read missing: err = %v, want ErrNotExist", err)
	}
}

func TestSeek(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "s.txt", []byte("0123456789"), "text/plain")
	f, _ := newFS().OpenRaw(testURL("s.txt"))
	defer func() { _ = f.Close() }()

	buf := make([]byte, 3)
	if _, err := io.ReadFull(f, buf); err != nil || string(buf) != "012" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	if pos, err := f.Seek(2, io.SeekCurrent); err != nil || pos != 5 {
		t.Fatalf("SeekCurrent = %d, %v", pos, err)
	}
	if _, err := io.ReadFull(f, buf); err != nil || string(buf) != "567" {
		t.Fatalf("read after SeekCurrent = %q, %v", buf, err)
	}
	if pos, err := f.Seek(-2, io.SeekEnd); err != nil || pos != 8 {
		t.Fatalf("SeekEnd = %d, %v", pos, err)
	}
	if rest := readAll(t, f); rest != "89" {
		t.Fatalf("read after SeekEnd = %q", rest)
	}
	if pos, err := f.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("SeekStart = %d, %v", pos, err)
	}
	if all := readAll(t, f); all != "0123456789" {
		t.Fatalf("read after rewind = %q", all)
	}
	// Past EOF reads return io.EOF.
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatalf("Seek past EOF: %v", err)
	}
	if n, err := f.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read past EOF = %d, %v", n, err)
	}
	if _, err := f.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("expected error for negative position")
	}
	if _, err := f.Seek(0, 42); err == nil {
		t.Fatal("expected error for bad whence")
	}
}

func TestSeek_DuringWrite(t *testing.T) {
	newTestServer(t)
	f, _ := newFS().OpenRaw(testURL("w.txt"))
	_, _ = f.Write([]byte("abc"))
	if pos, err := f.Seek(0, io.SeekCurrent); err != nil || pos != 3 {
		t.Fatalf("Seek(0, Current) = %d, %v", pos, err)
	}
	if _, err := f.Seek(0, io.SeekStart); !errors.Is(err, vfs.ErrNotSupported) {
		t.Fatalf("Seek during write: err = %v, want ErrNotSupported", err)
	}
	_ = f.Close()
}

func TestSeekEnd_Missing(t *testing.T) {
	newTestServer(t)
	f, _ := newFS().OpenRaw(testURL("missing"))
	if _, err := f.Seek(0, io.SeekEnd); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestReadRange(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "r.txt", []byte("abcdefghij"), "text/plain")
	f, _ := newFS().OpenRaw(testURL("r.txt"))
	ctx := context.Background()

	cases := []struct {
		off, length int64
		want        string
	}{
		{0, 3, "abc"},
		{3, 4, "defg"},
		{8, 10, "ij"},
		{5, 0, "fghij"},
		{0, 0, "abcdefghij"},
	}
	for _, c := range cases {
		got, err := vfs.ReadRange(ctx, f, c.off, c.length)
		if err != nil || string(got) != c.want {
			t.Errorf("ReadRange(%d,%d) = %q, %v; want %q", c.off, c.length, got, err, c.want)
		}
	}
	if _, err := vfs.ReadRange(ctx, f, 10, 5); err != io.EOF {
		t.Errorf("ReadRange past EOF err = %v, want io.EOF", err)
	}
	if _, err := vfs.ReadRange(ctx, f, -1, 5); err == nil {
		t.Error("expected error for negative offset")
	}
	if _, err := vfs.ReadRange(ctx, f, 0, -5); err == nil {
		t.Error("expected error for negative length")
	}
	missing, _ := newFS().OpenRaw(testURL("missing"))
	if _, err := vfs.ReadRange(ctx, missing, 0, 5); !errors.Is(err, vfs.ErrNotExist) {
		t.Errorf("missing blob err = %v, want ErrNotExist", err)
	}
}

func TestInfo(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "docs/readme.md", []byte("# hi"), "text/markdown")
	fs := newFS()

	f, _ := fs.OpenRaw(testURL("docs/readme.md"))
	info, err := f.Info()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.IsDir() || info.Size() != 4 || info.Name() != "docs/readme.md" || info.ModTime().IsZero() {
		t.Fatalf("unexpected file info: %v", info)
	}
	bi := info.(*BlobFileInfo)
	if bi.ContentType() != "text/markdown" || bi.ETag() == "" {
		t.Fatalf("content type / etag = %q / %q", bi.ContentType(), bi.ETag())
	}
	if f.ContentType() != "text/markdown" {
		t.Fatalf("ContentType after Info = %q", f.ContentType())
	}

	// Virtual directory (no marker blob, but children exist).
	d, _ := fs.OpenRaw(testURL("docs"))
	if info, err := d.Info(); err != nil || !info.IsDir() {
		t.Fatalf("virtual dir info = %v, %v", info, err)
	}
	// Trailing slash and container root are directories without a call.
	for _, raw := range []string{testURL("docs/"), "azblob://" + blobtest.AccountName + "/" + testContainer} {
		d, _ := fs.OpenRaw(raw)
		if info, err := d.Info(); err != nil || !info.IsDir() {
			t.Fatalf("%s info = %v, %v", raw, info, err)
		}
	}
	// Missing.
	m, _ := fs.OpenRaw(testURL("nothing"))
	if _, err := m.Info(); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("missing info err = %v", err)
	}
	// Non-404 errors surface mapped.
	srv.InjectError(http.MethodHead, http.StatusForbidden, "AuthorizationFailure")
	if _, err := f.Info(); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("403 info err = %v, want ErrPermission", err)
	}
}

func TestMkdir(t *testing.T) {
	srv := newTestServer(t)
	fs := newFS()

	d, err := fs.MkdirRaw(testURL("a/b"))
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if d.Url().String() != testURL("a/b/") {
		t.Fatalf("dir url = %s", d.Url())
	}
	if _, ok := srv.GetBlob(testContainer, "a/b/"); !ok {
		t.Fatal("marker blob a/b/ not created")
	}
	if info, _ := d.Info(); !info.IsDir() {
		t.Fatal("marker should report IsDir")
	}

	// MkdirAll creates a missing container on demand.
	if _, err := fs.MkdirAllRaw("azblob://" + blobtest.AccountName + "/newc/x/"); err != nil {
		t.Fatalf("MkdirAll new container: %v", err)
	}
	if !srv.HasContainer("newc") {
		t.Fatal("container newc not created")
	}
	// Container root mkdir creates (or tolerates) the container.
	if _, err := fs.MkdirAllRaw("azblob://" + blobtest.AccountName + "/other"); err != nil {
		t.Fatalf("Mkdir container: %v", err)
	}
	if _, err := fs.MkdirAllRaw("azblob://" + blobtest.AccountName + "/other"); err != nil {
		t.Fatalf("Mkdir existing container: %v", err)
	}
	if !srv.HasContainer("other") {
		t.Fatal("container other not created")
	}
	srv.InjectError(http.MethodPut, http.StatusForbidden, "AuthorizationPermissionMismatch")
	if _, err := fs.MkdirAllRaw("azblob://" + blobtest.AccountName + "/third"); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("container create 403: %v", err)
	}
	srv.InjectError(http.MethodPut, http.StatusForbidden, "AuthorizationPermissionMismatch")
	if _, err := fs.MkdirAllRaw(testURL("denied/")); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("marker 403: %v", err)
	}
	if _, err := fs.MkdirRaw("azblob://acct"); err == nil {
		t.Fatal("expected error without container")
	}
}

func seedTree(srv *blobtest.Server) {
	for _, k := range []string{
		"tree/a.txt", "tree/b.csv", "tree/sub/", "tree/sub/c.csv", "tree/sub/deep/d.log", "tree/z/e.txt", "other.txt",
	} {
		srv.PutBlob(testContainer, k, []byte("content of "+k), "text/plain")
	}
}

func TestList(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	fs := newFS()

	for _, raw := range []string{testURL("tree"), testURL("tree/")} {
		files, err := fs.ListRaw(raw)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got := keysOf(files)
		want := []string{"tree/a.txt", "tree/b.csv", "tree/sub/", "tree/z/"}
		if len(got) != len(want) {
			t.Fatalf("List(%s) = %v, want %v", raw, got, want)
		}
		for _, k := range want {
			if !got[k] {
				t.Fatalf("List(%s) missing %s: %v", raw, k, got)
			}
		}
	}

	// Container root listing.
	files, err := fs.ListRaw("azblob://" + blobtest.AccountName + "/" + testContainer)
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if got := keysOf(files); len(got) != 2 || !got["tree/"] || !got["other.txt"] {
		t.Fatalf("root list = %v", got)
	}

	// Pagination.
	srv.SetPageSize(1)
	files, err = fs.ListRaw(testURL("tree/"))
	if err != nil || len(files) != 4 {
		t.Fatalf("paginated list = %d files, %v", len(files), err)
	}

	// Missing container.
	if _, err := fs.ListRaw("azblob://" + blobtest.AccountName + "/nope/"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("missing container list err = %v", err)
	}
}

func TestListIter(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	srv.SetPageSize(2)
	fs := newFS()
	ctx := context.Background()

	it, err := vfs.ListIter(ctx, fs, mustURL(t, testURL("tree/")))
	if err != nil {
		t.Fatalf("ListIter: %v", err)
	}
	var keys []string
	for {
		f, err := it.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		keys = append(keys, f.(*BlobFile).urlOpts.Key)
	}
	if len(keys) != 4 {
		t.Fatalf("iter keys = %v", keys)
	}
	if _, err := it.Next(ctx); err != io.EOF {
		t.Fatalf("Next after EOF = %v", err)
	}
	_ = it.Close()

	it2, _ := fs.ListIter(ctx, mustURL(t, testURL("tree/")))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := it2.Next(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next with cancelled ctx = %v", err)
	}
	if err := it2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := it2.Next(ctx); err != io.EOF {
		t.Fatalf("Next after Close = %v", err)
	}
	if _, err := fs.ListIter(ctx, mustURL(t, "azblob://")); err == nil {
		t.Fatal("expected error for URL without account")
	}
}

func TestListAll(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	f, _ := newFS().OpenRaw(testURL("tree"))
	files, err := f.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	got := keysOf(files)
	for _, k := range []string{"tree/a.txt", "tree/b.csv", "tree/sub/", "tree/sub/c.csv", "tree/sub/deep/d.log", "tree/z/e.txt"} {
		if !got[k] {
			t.Fatalf("ListAll missing %s: %v", k, got)
		}
	}
	if len(got) != 6 {
		t.Fatalf("ListAll = %v", got)
	}
}

func TestWalk(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	fs := newFS()

	var all []string
	if err := fs.WalkRaw(testURL("tree"), func(f vfs.VFile) error {
		all = append(all, f.(*BlobFile).urlOpts.Key)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("walk = %v", all)
	}

	// SkipDir on a directory marker skips its subtree.
	var visited []string
	err := fs.WalkRaw(testURL("tree/"), func(f vfs.VFile) error {
		k := f.(*BlobFile).urlOpts.Key
		visited = append(visited, k)
		if k == "tree/sub/" {
			return vfs.ErrSkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range visited {
		if strings.HasPrefix(k, "tree/sub/") && k != "tree/sub/" {
			t.Fatalf("SkipDir did not skip %s (visited %v)", k, visited)
		}
	}
	if !contains(visited, "tree/z/e.txt") {
		t.Fatalf("walk stopped too early: %v", visited)
	}

	// SkipDir on a nested file skips the rest of its directory only.
	visited = nil
	_ = fs.WalkRaw(testURL("tree/"), func(f vfs.VFile) error {
		k := f.(*BlobFile).urlOpts.Key
		visited = append(visited, k)
		if k == "tree/sub/c.csv" {
			return vfs.ErrSkipDir
		}
		return nil
	})
	if contains(visited, "tree/sub/deep/d.log") || !contains(visited, "tree/z/e.txt") {
		t.Fatalf("file SkipDir visited %v", visited)
	}

	// SkipDir on a top-level file ends the walk.
	visited = nil
	_ = fs.WalkRaw(testURL("tree/"), func(f vfs.VFile) error {
		visited = append(visited, f.(*BlobFile).urlOpts.Key)
		return vfs.ErrSkipDir
	})
	if len(visited) != 1 {
		t.Fatalf("top-level SkipDir visited %v", visited)
	}

	// SkipAll stops without error.
	count := 0
	if err := fs.WalkRaw(testURL("tree/"), func(vfs.VFile) error {
		count++
		return vfs.ErrSkipAll
	}); err != nil || count != 1 {
		t.Fatalf("SkipAll: count=%d err=%v", count, err)
	}

	// Other errors propagate.
	boom := errors.New("boom")
	if err := fs.WalkRaw(testURL("tree/"), func(vfs.VFile) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("walk err = %v, want boom", err)
	}

	// Cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fs.WalkCtx(ctx, mustURL(t, testURL("tree/")), func(vfs.VFile) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("WalkCtx cancelled err = %v", err)
	}
	if err := fs.WalkRaw("bad://x", func(vfs.VFile) error { return nil }); err == nil {
		t.Fatal("expected scheme error")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestFindAndDeleteMatching(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	fs := newFS()
	loc := mustURL(t, testURL("tree/"))
	csv := func(f vfs.VFile) (bool, error) {
		return strings.HasSuffix(f.Url().Path, ".csv"), nil
	}
	found, err := fs.Find(loc, csv)
	if err != nil || len(found) != 2 {
		t.Fatalf("Find = %d, %v", len(found), err)
	}
	boom := errors.New("filter boom")
	if _, err := fs.Find(loc, func(vfs.VFile) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("Find filter err = %v", err)
	}
	if err := fs.DeleteMatching(loc, csv); err != nil {
		t.Fatalf("DeleteMatching: %v", err)
	}
	for _, k := range []string{"tree/b.csv", "tree/sub/c.csv"} {
		if _, ok := srv.GetBlob(testContainer, k); ok {
			t.Fatalf("%s not deleted", k)
		}
	}
	if _, ok := srv.GetBlob(testContainer, "tree/a.txt"); !ok {
		t.Fatal("non-matching blob deleted")
	}
	if err := fs.DeleteMatching(loc, func(vfs.VFile) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("DeleteMatching filter err = %v", err)
	}
}

func TestDelete(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	fs := newFS()

	if err := fs.DeleteRaw(testURL("tree/a.txt")); err != nil {
		t.Fatalf("Delete file: %v", err)
	}
	if _, ok := srv.GetBlob(testContainer, "tree/a.txt"); ok {
		t.Fatal("file not deleted")
	}
	if err := fs.DeleteRaw(testURL("tree/a.txt")); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("Delete missing err = %v", err)
	}
	// Directory without trailing slash (virtual + marker).
	if err := fs.DeleteRaw(testURL("tree/sub")); err != nil {
		t.Fatalf("Delete dir: %v", err)
	}
	for _, k := range srv.BlobNames(testContainer) {
		if strings.HasPrefix(k, "tree/sub") {
			t.Fatalf("%s survived dir delete", k)
		}
	}
	if _, ok := srv.GetBlob(testContainer, "tree/b.csv"); !ok {
		t.Fatal("sibling deleted")
	}

	// File.Delete / DeleteAll directly.
	f, _ := fs.OpenRaw(testURL("other.txt"))
	if err := f.DeleteAll(); err != nil {
		t.Fatalf("DeleteAll on file: %v", err)
	}
	if _, ok := srv.GetBlob(testContainer, "other.txt"); ok {
		t.Fatal("DeleteAll on file should delete the file")
	}

	// Container root delete removes every blob but keeps the container.
	if err := fs.DeleteRaw("azblob://" + blobtest.AccountName + "/" + testContainer + "/"); err != nil {
		t.Fatalf("Delete root: %v", err)
	}
	if n := len(srv.BlobNames(testContainer)); n != 0 || !srv.HasContainer(testContainer) {
		t.Fatalf("after root delete: %d blobs, container=%v", n, srv.HasContainer(testContainer))
	}

	srv.PutBlob(testContainer, "p.txt", nil, "")
	srv.InjectError(http.MethodDelete, http.StatusForbidden, "AuthorizationFailure")
	if err := fs.DeleteRaw(testURL("p.txt")); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("Delete 403 err = %v", err)
	}
	srv.PutBlob(testContainer, "d/x", nil, "")
	srv.InjectError(http.MethodDelete, http.StatusForbidden, "AuthorizationFailure")
	if err := fs.DeleteRaw(testURL("d/")); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("DeleteAll 403 err = %v", err)
	}
	srv.InjectError(http.MethodGet, http.StatusForbidden, "AuthorizationFailure")
	if err := fs.DeleteRaw(testURL("d/")); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("DeleteAll list 403 err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fs.DeleteCtx(ctx, mustURL(t, testURL("d/"))); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestCopyFile(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "src.txt", []byte("payload"), "text/plain")
	fs := newFS()

	if err := fs.CopyRaw(testURL("src.txt"), testURL("dst/copy.txt")); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if data, ok := srv.GetBlob(testContainer, "dst/copy.txt"); !ok || string(data) != "payload" {
		t.Fatalf("copied = %q, %v", data, ok)
	}
	if _, ok := srv.GetBlob(testContainer, "src.txt"); !ok {
		t.Fatal("source removed by copy")
	}
	if err := fs.CopyRaw(testURL("missing.txt"), testURL("x")); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("copy missing err = %v", err)
	}
	if err := fs.CopyRaw("bad://x", testURL("x")); err == nil {
		t.Fatal("expected src scheme error")
	}
	if err := fs.CopyRaw(testURL("src.txt"), "bad://x"); err == nil {
		t.Fatal("expected dst scheme error")
	}
}

func TestCopy_PendingIsAwaited(t *testing.T) {
	old := copyPollInterval
	copyPollInterval = time.Millisecond
	t.Cleanup(func() { copyPollInterval = old })

	srv := newTestServer(t)
	srv.PutBlob(testContainer, "big.bin", []byte("big"), "")
	srv.SetCopyPending(2)
	if err := newFS().CopyRaw(testURL("big.bin"), testURL("big-copy.bin")); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if srv.RequestCount(http.MethodHead) < 3 {
		t.Fatalf("expected polling HEADs, got %d", srv.RequestCount(http.MethodHead))
	}

	// Cancelled while pending.
	srv.SetCopyPending(1000)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := newFS().CopyCtx(ctx, mustURL(t, testURL("big.bin")), mustURL(t, testURL("big-copy2.bin"))); err == nil {
		t.Fatal("expected ctx error while copy pending")
	}
}

func TestCopy_StreamFallback(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "s.json", []byte(`{"a":1}`), "application/json")
	srv.RejectCopy(true)
	if err := newFS().CopyRaw(testURL("s.json"), testURL("t.json")); err != nil {
		t.Fatalf("Copy with fallback: %v", err)
	}
	if data, _ := srv.GetBlob(testContainer, "t.json"); string(data) != `{"a":1}` {
		t.Fatalf("fallback content = %q", data)
	}
	if ct := srv.BlobContentType(testContainer, "t.json"); ct != "application/json" {
		t.Fatalf("fallback content type = %q", ct)
	}
}

func TestCopyDirAndMove(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	fs := newFS()

	if err := fs.CopyRaw(testURL("tree"), testURL("backup")); err != nil {
		t.Fatalf("Copy dir: %v", err)
	}
	var got []string
	for _, k := range srv.BlobNames(testContainer) {
		if strings.HasPrefix(k, "backup/") {
			got = append(got, k)
		}
	}
	sort.Strings(got)
	want := []string{"backup/a.txt", "backup/b.csv", "backup/sub/", "backup/sub/c.csv", "backup/sub/deep/d.log", "backup/z/e.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("copied dir = %v, want %v", got, want)
	}

	if err := fs.MoveRaw(testURL("backup/"), testURL("archive/")); err != nil {
		t.Fatalf("Move dir: %v", err)
	}
	for _, k := range srv.BlobNames(testContainer) {
		if strings.HasPrefix(k, "backup/") {
			t.Fatalf("%s left after move", k)
		}
	}
	if _, ok := srv.GetBlob(testContainer, "archive/sub/deep/d.log"); !ok {
		t.Fatal("moved tree incomplete")
	}

	// Move a file; moving onto itself is a no-op.
	if err := fs.MoveRaw(testURL("other.txt"), testURL("moved.txt")); err != nil {
		t.Fatalf("Move file: %v", err)
	}
	if _, ok := srv.GetBlob(testContainer, "other.txt"); ok {
		t.Fatal("source survived move")
	}
	if err := fs.MoveRaw(testURL("moved.txt"), testURL("moved.txt")); err != nil {
		t.Fatalf("self move: %v", err)
	}
	if _, ok := srv.GetBlob(testContainer, "moved.txt"); !ok {
		t.Fatal("self move deleted the blob")
	}
	if err := fs.MoveRaw("bad://x", "bad://x"); err == nil {
		t.Fatal("expected scheme error on self move")
	}
	if err := fs.MoveRaw(testURL("nope"), testURL("nope2")); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("move missing err = %v", err)
	}
}

func TestCopy_CrossAccountStream(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "x.txt", []byte("cross"), "text/plain")

	// Second "account" served by its own fake; copy must stream across.
	other := blobtest.NewServer()
	defer other.Close()
	other.CreateContainer("dst")
	registerAccount(t, "otheraccount", other)

	src := testURL("x.txt")
	dst := "azblob://otheraccount/dst/y.txt"
	if err := newFS().CopyRaw(src, dst); err != nil {
		t.Fatalf("cross-account copy: %v", err)
	}
	if data, ok := other.GetBlob("dst", "y.txt"); !ok || string(data) != "cross" {
		t.Fatalf("cross-account content = %q, %v", data, ok)
	}
}

func TestProperties(t *testing.T) {
	srv := newTestServer(t)
	srv.PutBlob(testContainer, "m.txt", []byte("m"), "text/plain")
	f, _ := newFS().OpenRaw(testURL("m.txt"))

	if err := f.AddProperty("department", "engineering"); err != nil {
		t.Fatalf("AddProperty: %v", err)
	}
	if err := f.AddProperty("Owner", "alice"); err != nil {
		t.Fatalf("AddProperty: %v", err)
	}
	// Re-adding with different case replaces.
	if err := f.AddProperty("DEPARTMENT", "research"); err != nil {
		t.Fatalf("AddProperty replace: %v", err)
	}
	md := srv.BlobMetadata(testContainer, "m.txt")
	if md["department"] != "research" || md["owner"] != "alice" || len(md) != 2 {
		t.Fatalf("metadata = %v", md)
	}
	if v, err := f.GetProperty("Department"); err != nil || v != "research" {
		t.Fatalf("GetProperty = %q, %v", v, err)
	}
	if _, err := f.GetProperty("missing"); err == nil {
		t.Fatal("expected error for missing key")
	}
	if data, _ := srv.GetBlob(testContainer, "m.txt"); string(data) != "m" {
		t.Fatal("content changed by metadata update")
	}

	missing, _ := newFS().OpenRaw(testURL("nope"))
	if err := missing.AddProperty("a", "b"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("AddProperty missing err = %v", err)
	}
	if _, err := missing.GetProperty("a"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("GetProperty missing err = %v", err)
	}
	srv.InjectError(http.MethodPut, http.StatusPreconditionFailed, "ConditionNotMet")
	if err := f.AddProperty("x", "y"); err == nil {
		t.Fatal("expected error on concurrent metadata change")
	}
}

func TestParent(t *testing.T) {
	newTestServer(t)
	fs := newFS()
	cases := map[string]string{
		testURL("a/b/c.txt"): testURL("a/b/"),
		testURL("a/b/"):      testURL("a/"),
		testURL("top.txt"):   testURL(""),
	}
	for in, want := range cases {
		f, _ := fs.OpenRaw(in)
		p, err := f.Parent()
		if err != nil {
			t.Fatalf("Parent(%s): %v", in, err)
		}
		if p.Url().String() != want {
			t.Errorf("Parent(%s) = %s, want %s", in, p.Url(), want)
		}
	}
}

func TestContentTypeDefault(t *testing.T) {
	newTestServer(t)
	f, _ := newFS().OpenRaw(testURL("x"))
	if f.ContentType() != defaultContentType {
		t.Fatalf("ContentType = %q", f.ContentType())
	}
}

func TestCloseUploadError(t *testing.T) {
	srv := newTestServer(t)
	f, _ := newFS().OpenRaw(testURL("fail.txt"))
	_, _ = f.WriteString("data")
	srv.InjectError(http.MethodPut, http.StatusForbidden, "AuthorizationFailure")
	if err := f.Close(); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("Close err = %v, want ErrPermission", err)
	}
}

func TestCtxVariants(t *testing.T) {
	srv := newTestServer(t)
	seedTree(srv)
	fs := newFS()
	ctx := context.Background()

	f, err := vfs.CreateCtx(ctx, fs, mustURL(t, testURL("ctx/new.txt")))
	if err != nil {
		t.Fatalf("CreateCtx: %v", err)
	}
	_ = f.Close()
	if _, err := vfs.OpenCtx(ctx, fs, mustURL(t, testURL("ctx/new.txt"))); err != nil {
		t.Fatalf("OpenCtx: %v", err)
	}
	if _, err := vfs.MkdirAllCtx(ctx, fs, mustURL(t, testURL("ctx/dir"))); err != nil {
		t.Fatalf("MkdirAllCtx: %v", err)
	}
	files, err := vfs.ListCtx(ctx, fs, mustURL(t, testURL("ctx/")))
	if err != nil || len(files) != 2 {
		t.Fatalf("ListCtx = %d, %v", len(files), err)
	}
	if err := vfs.CopyCtx(ctx, fs, mustURL(t, testURL("ctx/new.txt")), mustURL(t, testURL("ctx/c.txt"))); err != nil {
		t.Fatalf("CopyCtx: %v", err)
	}
	if err := vfs.MoveCtx(ctx, fs, mustURL(t, testURL("ctx/c.txt")), mustURL(t, testURL("ctx/m.txt"))); err != nil {
		t.Fatalf("MoveCtx: %v", err)
	}
	n := 0
	if err := vfs.WalkCtx(ctx, fs, mustURL(t, testURL("ctx/")), func(vfs.VFile) error { n++; return nil }); err != nil || n != 3 {
		t.Fatalf("WalkCtx n=%d err=%v", n, err)
	}
	if err := vfs.DeleteCtx(ctx, fs, mustURL(t, testURL("ctx/"))); err != nil {
		t.Fatalf("DeleteCtx: %v", err)
	}
	for _, k := range srv.BlobNames(testContainer) {
		if strings.HasPrefix(k, "ctx/") {
			t.Fatalf("%s survived DeleteCtx", k)
		}
	}

	// Invalid URLs are rejected by every ctx method.
	bad := mustURL(t, "bad://x/y")
	if _, err := fs.OpenCtx(ctx, bad); err == nil {
		t.Error("OpenCtx bad url")
	}
	if _, err := fs.MkdirAllCtx(ctx, bad); err == nil {
		t.Error("MkdirAllCtx bad url")
	}
	if err := fs.DeleteCtx(ctx, bad); err == nil {
		t.Error("DeleteCtx bad url")
	}
	if _, err := fs.ListCtx(ctx, bad); err == nil {
		t.Error("ListCtx bad url")
	}
	if err := fs.MoveCtx(ctx, bad, mustURL(t, testURL("x"))); err == nil {
		t.Error("MoveCtx bad url")
	}
}

func TestRawParseErrors(t *testing.T) {
	fs := newFS()
	bad := "://bad"
	if err := fs.CopyRaw(bad, "azblob://a/c/x"); err == nil {
		t.Error("CopyRaw src")
	}
	if err := fs.CopyRaw("azblob://a/c/x", bad); err == nil {
		t.Error("CopyRaw dst")
	}
	if err := fs.MoveRaw(bad, "azblob://a/c/x"); err == nil {
		t.Error("MoveRaw src")
	}
	if err := fs.MoveRaw("azblob://a/c/x", bad); err == nil {
		t.Error("MoveRaw dst")
	}
	if err := fs.DeleteRaw(bad); err == nil {
		t.Error("DeleteRaw")
	}
	if _, err := fs.ListRaw(bad); err == nil {
		t.Error("ListRaw")
	}
	if _, err := fs.MkdirRaw(bad); err == nil {
		t.Error("MkdirRaw")
	}
	if _, err := fs.MkdirAllRaw(bad); err == nil {
		t.Error("MkdirAllRaw")
	}
	if _, err := fs.OpenRaw(bad); err == nil {
		t.Error("OpenRaw")
	}
	if err := fs.WalkRaw(bad, nil); err == nil {
		t.Error("WalkRaw")
	}
}
