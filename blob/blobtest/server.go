// Package blobtest provides an in-memory fake of the Azure Blob Storage
// REST API, served over net/http/httptest, for offline tests of code that
// uses the Azure SDK for Go (azblob) — including the golly-azure blob VFS
// and chrono/bloblock packages.
//
// The fake speaks enough of the Blob service protocol for the SDK's block
// blob, blob and container clients:
//
//   - Create / Delete / Get Properties of a container
//   - List Blobs (flat and hierarchical, prefix, delimiter, pagination)
//   - Put Blob, Put Block, Put Block List
//   - Get Blob (with Range), Get Blob Properties (HEAD), Delete Blob
//   - Set Blob Metadata
//   - Copy Blob (server-side, same server)
//   - If-Match / If-None-Match preconditions on writes, reads and deletes
//
// Authentication headers are accepted but never verified, so a client
// built with the well-known Azurite shared key (AccountName / AccountKey)
// or ConnectionString() works out of the box:
//
//	srv := blobtest.NewServer()
//	defer srv.Close()
//	srv.CreateContainer("data")
//	client, _ := azblob.NewClientFromConnectionString(srv.ConnectionString(), nil)
package blobtest

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// AccountName is the storage account name served by the fake (the Azurite default).
	AccountName = "devstoreaccount1"
	// AccountKey is the well-known Azurite shared key. The fake does not verify signatures.
	AccountKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

type fakeBlob struct {
	data        []byte
	contentType string
	metadata    map[string]string
	etag        string
	lastMod     time.Time
	copyPending int
}

type fakeContainer struct {
	blobs  map[string]*fakeBlob
	staged map[string]map[string][]byte
	etag   string
}

type injectedErr struct {
	method string
	status int
	code   string
}

// Server is an in-memory Azure Blob Storage fake. It is safe for concurrent use.
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	containers  map[string]*fakeContainer
	gen         int64
	injected    []injectedErr
	rejectCopy  bool
	copyPending int
	pageSize    int
	requests    map[string]int
}

// NewServer starts a new fake Blob service. Call Close when done.
func NewServer() *Server {
	s := &Server{
		containers: make(map[string]*fakeContainer),
		requests:   make(map[string]int),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// ServiceURL returns the Blob service endpoint (path-style, Azurite layout):
// http://127.0.0.1:<port>/devstoreaccount1
func (s *Server) ServiceURL() string {
	return s.URL + "/" + AccountName
}

// ConnectionString returns a storage connection string pointing at the fake.
func (s *Server) ConnectionString() string {
	return fmt.Sprintf("DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;BlobEndpoint=%s;",
		AccountName, AccountKey, s.ServiceURL())
}

// CreateContainer creates a container (no-op if it exists).
func (s *Server) CreateContainer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.containers[name]; !ok {
		s.containers[name] = s.newContainer()
	}
}

// HasContainer reports whether the container exists.
func (s *Server) HasContainer(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.containers[name]
	return ok
}

// PutBlob stores a blob directly, creating the container if needed.
func (s *Server) PutBlob(containerName, name string, data []byte, contentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[containerName]
	if !ok {
		c = s.newContainer()
		s.containers[containerName] = c
	}
	c.blobs[name] = &fakeBlob{
		data:        append([]byte(nil), data...),
		contentType: contentType,
		metadata:    map[string]string{},
		etag:        s.nextETag(),
		lastMod:     time.Now().UTC(),
	}
}

// GetBlob returns a copy of a blob's content.
func (s *Server) GetBlob(containerName, name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.lookup(containerName, name)
	if b == nil {
		return nil, false
	}
	return append([]byte(nil), b.data...), true
}

// BlobContentType returns a blob's stored Content-Type.
func (s *Server) BlobContentType(containerName, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.lookup(containerName, name); b != nil {
		return b.contentType
	}
	return ""
}

// BlobMetadata returns a copy of a blob's metadata (keys lower-cased).
func (s *Server) BlobMetadata(containerName, name string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	if b := s.lookup(containerName, name); b != nil {
		for k, v := range b.metadata {
			out[k] = v
		}
	}
	return out
}

// BlobNames returns the sorted blob names in a container.
func (s *Server) BlobNames(containerName string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[containerName]
	if !ok {
		return nil
	}
	return sortedNames(c.blobs)
}

// InjectError makes the next request with the given HTTP method ("" for
// any method) fail with status and error code. Injections queue up and
// are consumed in order.
func (s *Server) InjectError(method string, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected = append(s.injected, injectedErr{method: method, status: status, code: code})
}

// RejectCopy makes server-side Copy Blob fail with 403 CannotVerifyCopySource,
// as Azure does for cross-account copies without a SAS on the source.
func (s *Server) RejectCopy(reject bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectCopy = reject
}

// SetCopyPending makes subsequent server-side copies report
// x-ms-copy-status: pending for n property reads before succeeding.
func (s *Server) SetCopyPending(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.copyPending = n
}

// SetPageSize caps the number of entries returned per List Blobs page
// (0 restores the service default of 5000), to exercise pagination.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// RequestCount returns how many requests with the given method were served.
func (s *Server) RequestCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[method]
}

func (s *Server) newContainer() *fakeContainer {
	return &fakeContainer{
		blobs:  map[string]*fakeBlob{},
		staged: map[string]map[string][]byte{},
		etag:   s.nextETag(),
	}
}

func (s *Server) nextETag() string {
	s.gen++
	return fmt.Sprintf("\"0x8D%012X\"", s.gen)
}

func (s *Server) lookup(containerName, name string) *fakeBlob {
	c, ok := s.containers[containerName]
	if !ok {
		return nil
	}
	return c.blobs[name]
}

func sortedNames(m map[string]*fakeBlob) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// splitPath splits /<account>/<container>/<blob...> into container and blob.
func splitPath(p string) (containerName, blobName string) {
	p = strings.TrimPrefix(p, "/")
	_, rest, _ := strings.Cut(p, "/") // drop account
	containerName, blobName, _ = strings.Cut(rest, "/")
	return
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests[r.Method]++
	w.Header().Set("x-ms-request-id", strconv.FormatInt(s.gen, 10))
	w.Header().Set("x-ms-version", "2025-01-05")
	w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))

	for i, inj := range s.injected {
		if inj.method == "" || inj.method == r.Method {
			s.injected = append(s.injected[:i], s.injected[i+1:]...)
			writeError(w, r, inj.status, inj.code)
			return
		}
	}

	containerName, blobName := splitPath(r.URL.Path)
	q := r.URL.Query()
	if containerName == "" {
		writeError(w, r, http.StatusBadRequest, "InvalidUri")
		return
	}
	if blobName == "" && q.Get("restype") == "container" {
		s.handleContainer(w, r, containerName, q)
		return
	}
	c, ok := s.containers[containerName]
	if !ok {
		writeError(w, r, http.StatusNotFound, "ContainerNotFound")
		return
	}
	s.handleBlob(w, r, c, blobName, q)
}

func (s *Server) handleContainer(w http.ResponseWriter, r *http.Request, name string, q url.Values) {
	c, exists := s.containers[name]
	switch {
	case r.Method == http.MethodPut:
		if exists {
			writeError(w, r, http.StatusConflict, "ContainerAlreadyExists")
			return
		}
		c = s.newContainer()
		s.containers[name] = c
		w.Header().Set("ETag", c.etag)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		if !exists {
			writeError(w, r, http.StatusNotFound, "ContainerNotFound")
			return
		}
		delete(s.containers, name)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet && q.Get("comp") == "list":
		if !exists {
			writeError(w, r, http.StatusNotFound, "ContainerNotFound")
			return
		}
		s.listBlobs(w, r, name, c, q)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		if !exists {
			writeError(w, r, http.StatusNotFound, "ContainerNotFound")
			return
		}
		w.Header().Set("ETag", c.etag)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "UnsupportedHttpVerb")
	}
}

type xmlProps struct {
	LastModified  string `xml:"Last-Modified"`
	Etag          string `xml:"Etag"`
	ContentLength int64  `xml:"Content-Length"`
	ContentType   string `xml:"Content-Type"`
	BlobType      string `xml:"BlobType"`
}

type xmlBlob struct {
	Name       string   `xml:"Name"`
	Properties xmlProps `xml:"Properties"`
}

type xmlPrefix struct {
	Name string `xml:"Name"`
}

type xmlEnum struct {
	XMLName         xml.Name `xml:"EnumerationResults"`
	ServiceEndpoint string   `xml:"ServiceEndpoint,attr"`
	ContainerName   string   `xml:"ContainerName,attr"`
	Prefix          string   `xml:"Prefix,omitempty"`
	Marker          string   `xml:"Marker,omitempty"`
	MaxResults      int      `xml:"MaxResults,omitempty"`
	Delimiter       string   `xml:"Delimiter,omitempty"`
	Blobs           struct {
		Blob       []xmlBlob   `xml:"Blob"`
		BlobPrefix []xmlPrefix `xml:"BlobPrefix"`
	} `xml:"Blobs"`
	NextMarker string `xml:"NextMarker"`
}

func (s *Server) listBlobs(w http.ResponseWriter, _ *http.Request, name string, c *fakeContainer, q url.Values) {
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	maxResults := 5000
	if v, err := strconv.Atoi(q.Get("maxresults")); err == nil && v > 0 {
		maxResults = v
	}
	if s.pageSize > 0 && s.pageSize < maxResults {
		maxResults = s.pageSize
	}
	start := 0
	if v, err := strconv.Atoi(q.Get("marker")); err == nil && v > 0 {
		start = v
	}

	type entry struct {
		name   string
		prefix bool
	}
	var entries []entry
	seen := map[string]bool{}
	for _, n := range sortedNames(c.blobs) {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if delim != "" {
			rest := n[len(prefix):]
			if i := strings.Index(rest, delim); i >= 0 {
				p := prefix + rest[:i+len(delim)]
				if !seen[p] {
					seen[p] = true
					entries = append(entries, entry{name: p, prefix: true})
				}
				continue
			}
		}
		entries = append(entries, entry{name: n})
	}

	out := xmlEnum{
		ServiceEndpoint: s.URL + "/" + AccountName + "/",
		ContainerName:   name,
		Prefix:          prefix,
		Marker:          q.Get("marker"),
		MaxResults:      maxResults,
		Delimiter:       delim,
	}
	end := start + maxResults
	if end < len(entries) {
		out.NextMarker = strconv.Itoa(end)
	} else {
		end = len(entries)
	}
	if start > len(entries) {
		start = len(entries)
	}
	for _, e := range entries[start:end] {
		if e.prefix {
			out.Blobs.BlobPrefix = append(out.Blobs.BlobPrefix, xmlPrefix{Name: e.name})
			continue
		}
		b := c.blobs[e.name]
		out.Blobs.Blob = append(out.Blobs.Blob, xmlBlob{Name: e.name, Properties: xmlProps{
			LastModified:  b.lastMod.Format(http.TimeFormat),
			Etag:          b.etag,
			ContentLength: int64(len(b.data)),
			ContentType:   b.contentType,
			BlobType:      "BlockBlob",
		}})
	}
	body, _ := xml.Marshal(out)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// checkConditions evaluates If-Match / If-None-Match against b (nil when absent).
func checkConditions(r *http.Request, b *fakeBlob) (int, string, bool) {
	write := r.Method == http.MethodPut || r.Method == http.MethodDelete
	if im := r.Header.Get("If-Match"); im != "" {
		if b == nil {
			if write && r.Method == http.MethodDelete {
				return http.StatusNotFound, "BlobNotFound", false
			}
			return http.StatusPreconditionFailed, "ConditionNotMet", false
		}
		if im != "*" && im != b.etag {
			return http.StatusPreconditionFailed, "ConditionNotMet", false
		}
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && b != nil {
		if inm == "*" {
			if write {
				return http.StatusConflict, "BlobAlreadyExists", false
			}
			return http.StatusNotModified, "ConditionNotMet", false
		}
		if inm == b.etag {
			if write {
				return http.StatusPreconditionFailed, "ConditionNotMet", false
			}
			return http.StatusNotModified, "ConditionNotMet", false
		}
	}
	return 0, "", true
}

func metadataFrom(h http.Header) map[string]string {
	md := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-ms-meta-") && len(v) > 0 {
			md[lk[len("x-ms-meta-"):]] = v[0]
		}
	}
	return md
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request, c *fakeContainer, name string, q url.Values) {
	b := c.blobs[name]
	if status, code, ok := checkConditions(r, b); !ok {
		writeError(w, r, status, code)
		return
	}
	switch r.Method {
	case http.MethodPut:
		switch q.Get("comp") {
		case "block":
			data, _ := io.ReadAll(r.Body)
			if c.staged[name] == nil {
				c.staged[name] = map[string][]byte{}
			}
			c.staged[name][q.Get("blockid")] = data
			w.WriteHeader(http.StatusCreated)
		case "blocklist":
			s.putBlockList(w, r, c, name)
		case "metadata":
			if b == nil {
				writeError(w, r, http.StatusNotFound, "BlobNotFound")
				return
			}
			b.metadata = metadataFrom(r.Header)
			b.etag = s.nextETag()
			b.lastMod = time.Now().UTC()
			writeBlobWriteHeaders(w, b, http.StatusOK)
		case "":
			if src := r.Header.Get("x-ms-copy-source"); src != "" {
				s.copyBlob(w, r, c, name, src)
				return
			}
			data, _ := io.ReadAll(r.Body)
			nb := &fakeBlob{
				data:        data,
				contentType: r.Header.Get("x-ms-blob-content-type"),
				metadata:    metadataFrom(r.Header),
				etag:        s.nextETag(),
				lastMod:     time.Now().UTC(),
			}
			if nb.contentType == "" {
				nb.contentType = "application/octet-stream"
			}
			c.blobs[name] = nb
			writeBlobWriteHeaders(w, nb, http.StatusCreated)
		default:
			writeError(w, r, http.StatusBadRequest, "UnsupportedQueryParameter")
		}
	case http.MethodGet, http.MethodHead:
		if b == nil {
			writeError(w, r, http.StatusNotFound, "BlobNotFound")
			return
		}
		s.getBlob(w, r, b)
	case http.MethodDelete:
		if b == nil {
			writeError(w, r, http.StatusNotFound, "BlobNotFound")
			return
		}
		delete(c.blobs, name)
		w.WriteHeader(http.StatusAccepted)
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "UnsupportedHttpVerb")
	}
}

type xmlBlockList struct {
	Items []struct {
		XMLName xml.Name
		ID      string `xml:",chardata"`
	} `xml:",any"`
}

func (s *Server) putBlockList(w http.ResponseWriter, r *http.Request, c *fakeContainer, name string) {
	raw, _ := io.ReadAll(r.Body)
	var bl xmlBlockList
	if err := xml.Unmarshal(raw, &bl); err != nil {
		writeError(w, r, http.StatusBadRequest, "InvalidXmlDocument")
		return
	}
	var buf bytes.Buffer
	staged := c.staged[name]
	for _, it := range bl.Items {
		data, ok := staged[it.ID]
		if !ok {
			if old := c.blobs[name]; old != nil && it.XMLName.Local != "Uncommitted" {
				// Re-committing the existing blob content is not tracked per block.
				continue
			}
			writeError(w, r, http.StatusBadRequest, "InvalidBlockList")
			return
		}
		buf.Write(data)
	}
	nb := &fakeBlob{
		data:        buf.Bytes(),
		contentType: r.Header.Get("x-ms-blob-content-type"),
		metadata:    metadataFrom(r.Header),
		etag:        s.nextETag(),
		lastMod:     time.Now().UTC(),
	}
	if nb.contentType == "" {
		nb.contentType = "application/octet-stream"
	}
	c.blobs[name] = nb
	delete(c.staged, name)
	writeBlobWriteHeaders(w, nb, http.StatusCreated)
}

func (s *Server) copyBlob(w http.ResponseWriter, r *http.Request, c *fakeContainer, name, src string) {
	if s.rejectCopy {
		writeError(w, r, http.StatusForbidden, "CannotVerifyCopySource")
		return
	}
	su, err := url.Parse(src)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "InvalidHeaderValue")
		return
	}
	sc, sn := splitPath(su.Path)
	sb := s.lookup(sc, sn)
	if sb == nil {
		writeError(w, r, http.StatusNotFound, "CannotVerifyCopySource")
		return
	}
	md := metadataFrom(r.Header)
	if len(md) == 0 {
		for k, v := range sb.metadata {
			md[k] = v
		}
	}
	nb := &fakeBlob{
		data:        append([]byte(nil), sb.data...),
		contentType: sb.contentType,
		metadata:    md,
		etag:        s.nextETag(),
		lastMod:     time.Now().UTC(),
		copyPending: s.copyPending,
	}
	c.blobs[name] = nb
	status := "success"
	if nb.copyPending > 0 {
		status = "pending"
	}
	w.Header().Set("x-ms-copy-id", strings.Trim(nb.etag, "\""))
	w.Header().Set("x-ms-copy-status", status)
	writeBlobWriteHeaders(w, nb, http.StatusAccepted)
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request, b *fakeBlob) {
	h := w.Header()
	h.Set("ETag", b.etag)
	h.Set("Last-Modified", b.lastMod.Format(http.TimeFormat))
	h.Set("Content-Type", b.contentType)
	h.Set("x-ms-blob-type", "BlockBlob")
	h.Set("Accept-Ranges", "bytes")
	for k, v := range b.metadata {
		h.Set("x-ms-meta-"+k, v)
	}
	if b.copyPending > 0 || r.Method == http.MethodHead {
		status := "success"
		if b.copyPending > 0 {
			status = "pending"
			b.copyPending--
		}
		h.Set("x-ms-copy-status", status)
	}

	data := b.data
	status := http.StatusOK
	rng := r.Header.Get("x-ms-range")
	if rng == "" {
		rng = r.Header.Get("Range")
	}
	if rng != "" {
		start, end, ok := parseRange(rng, int64(len(data)))
		if !ok {
			h.Del("Content-Type")
			writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data = data[start : end+1]
		status = http.StatusPartialContent
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// parseRange parses "bytes=start-[end]" against size.
func parseRange(v string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok {
		return 0, 0, false
	}
	a, bStr, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if bStr != "" {
		e, err := strconv.ParseInt(bStr, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		if e < end {
			end = e
		}
	}
	return start, end, true
}

func writeBlobWriteHeaders(w http.ResponseWriter, b *fakeBlob, status int) {
	w.Header().Set("ETag", b.etag)
	w.Header().Set("Last-Modified", b.lastMod.Format(http.TimeFormat))
	w.Header().Set("x-ms-request-server-encrypted", "true")
	w.WriteHeader(status)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("x-ms-error-code", code)
	if r.Method == http.MethodHead || status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "%s<Error><Code>%s</Code><Message>%s</Message></Error>", xml.Header, code, http.StatusText(status))
}
