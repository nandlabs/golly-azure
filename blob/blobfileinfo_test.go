package blob

import (
	"os"
	"testing"
	"time"
)

func TestBlobFileInfo(t *testing.T) {
	now := time.Now()
	fs := &BlobFS{}
	info := &BlobFileInfo{fs: fs, key: "path/file.txt", size: 1024, lastModified: now, contentType: "text/plain", etag: "\"e\""}
	if info.Name() != "path/file.txt" || info.Size() != 1024 || !info.ModTime().Equal(now) {
		t.Errorf("unexpected info: %v", info)
	}
	if info.Mode() != 0 || info.IsDir() {
		t.Error("file should have mode 0 and not be a dir")
	}
	if info.Sys() != fs {
		t.Error("Sys() should return the filesystem")
	}
	if info.ContentType() != "text/plain" || info.ETag() != "\"e\"" {
		t.Error("content type / etag")
	}
	if info.String() == "" {
		t.Error("expected non-empty String()")
	}
	dir := &BlobFileInfo{isDir: true}
	if !dir.IsDir() || dir.Mode() != os.ModeDir {
		t.Error("dir info")
	}
}

func TestBlobFS_Schemes(t *testing.T) {
	schemes := (&BlobFS{}).Schemes()
	if len(schemes) != 1 || schemes[0] != "azblob" {
		t.Errorf("expected ['azblob'], got %v", schemes)
	}
}
