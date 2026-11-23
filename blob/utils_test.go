package blob

import (
	"net/url"
	"testing"
)

func TestParseURL_Valid(t *testing.T) {
	u, _ := url.Parse("azblob://myaccount/mycontainer/path/to/file.txt")
	opts, err := parseURL(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Account != "myaccount" || opts.Container != "mycontainer" || opts.Key != "path/to/file.txt" {
		t.Errorf("parsed = %+v", opts)
	}
}

func TestParseURL_ContainerOnly(t *testing.T) {
	for _, raw := range []string{"azblob://acct/c", "azblob://acct/c/"} {
		u, _ := url.Parse(raw)
		opts, err := parseURL(u)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if opts.Container != "c" || opts.Key != "" {
			t.Errorf("%s parsed = %+v", raw, opts)
		}
	}
}

func TestParseURL_DirKey(t *testing.T) {
	u, _ := url.Parse("azblob://acct/c/dir/")
	opts, _ := parseURL(u)
	if opts.Key != "dir/" || !isDirKey(opts.Key) {
		t.Errorf("key = %q", opts.Key)
	}
}

func TestParseURL_Errors(t *testing.T) {
	if _, err := parseURL(nil); err == nil {
		t.Error("expected error for nil URL")
	}
	for _, raw := range []string{"s3://acct/c/k", "azblob:///c/k", "azblob://acct", "azblob://acct/"} {
		u, _ := url.Parse(raw)
		if _, err := parseURL(u); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

func TestDirHelpers(t *testing.T) {
	if !isDirKey("") || !isDirKey("a/") || isDirKey("a") {
		t.Error("isDirKey")
	}
	if dirPrefix("") != "" || dirPrefix("a") != "a/" || dirPrefix("a/") != "a/" {
		t.Error("dirPrefix")
	}
	o := &urlOpts{Account: "acct", Container: "c", Key: "x"}
	if got := o.withKey("y/z").u.String(); got != "azblob://acct/c/y/z" {
		t.Errorf("withKey url = %s", got)
	}
}

func TestContentTypeFor(t *testing.T) {
	if ct := contentTypeFor("a.json"); ct != "application/json" {
		t.Errorf("json = %q", ct)
	}
	if ct := contentTypeFor("noext"); ct != defaultContentType {
		t.Errorf("noext = %q", ct)
	}
}
