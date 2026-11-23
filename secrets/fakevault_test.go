package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"oss.nandlabs.io/golly-azure/azcfg"
)

const fakeVaultURL = "https://fakevault.vault.azure.net/"

// fakeVault is an in-memory Key Vault secrets REST API implemented as an
// azcore policy.Transporter. It performs the bearer challenge handshake the
// azsecrets client expects (401 + WWW-Authenticate on unauthenticated
// requests) so the real client pipeline is exercised end to end.
type fakeVault struct {
	mu       sync.Mutex
	secrets  map[string]*fakeSecret // lower-cased name -> secret
	deleted  map[string]*fakeSecret
	calls    map[string]int // "METHOD /first-segment" -> count
	pageSize int
	failWith int // when non-zero, every authenticated request fails with this status
}

type fakeVersion struct {
	id          string
	value       string
	contentType string
	tags        map[string]string
	created     time.Time
}

type fakeSecret struct {
	name     string
	versions []*fakeVersion
	managed  bool
}

func newFakeVault() *fakeVault {
	return &fakeVault{
		secrets:  map[string]*fakeSecret{},
		deleted:  map[string]*fakeSecret{},
		calls:    map[string]int{},
		pageSize: 2,
	}
}

func (f *fakeVault) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeVault) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.calls {
		n += v
	}
	return n
}

func (f *fakeVault) latest(name string) *fakeVersion {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.secrets[strings.ToLower(name)]
	if s == nil || len(s.versions) == 0 {
		return nil
	}
	return s.versions[len(s.versions)-1]
}

// seed stores a secret directly, bypassing the store.
func (f *fakeVault) seed(name, value string, tags map[string]string, managed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSecret{name: name, managed: managed}
	s.versions = append(s.versions, &fakeVersion{id: "v0", value: value, tags: tags, created: time.Unix(1700000000, 0)})
	f.secrets[strings.ToLower(name)] = s
}

func jsonResp(req *http.Request, status int, body interface{}) *http.Response {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(buf)),
		Request:    req,
	}
}

func kvError(req *http.Request, status int, code, msg string) *http.Response {
	return jsonResp(req, status, map[string]interface{}{
		"error": map[string]string{"code": code, "message": msg},
	})
}

func (f *fakeVault) bundle(s *fakeSecret, v *fakeVersion, withValue bool) map[string]interface{} {
	b := map[string]interface{}{
		"id": fakeVaultURL + "secrets/" + s.name + "/" + v.id,
		"attributes": map[string]interface{}{
			"enabled": true,
			"created": v.created.Unix(),
			"updated": v.created.Unix(),
		},
	}
	if withValue {
		b["value"] = v.value
	}
	if v.contentType != "" {
		b["contentType"] = v.contentType
	}
	if len(v.tags) > 0 {
		b["tags"] = v.tags
	}
	if s.managed {
		b["managed"] = true
	}
	return b
}

func (f *fakeVault) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		resp := jsonResp(req, http.StatusUnauthorized, nil)
		resp.Header.Set("WWW-Authenticate",
			`Bearer authorization="https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000", resource="https://vault.azure.net"`)
		return resp, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.Trim(req.URL.Path, "/")
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i], _ = url.PathUnescape(parts[i])
	}
	f.calls[req.Method+" "+parts[0]]++

	if f.failWith != 0 {
		return kvError(req, f.failWith, "Forced", "forced failure"), nil
	}

	switch {
	case parts[0] == "secrets" && len(parts) == 1 && req.Method == http.MethodGet:
		return f.list(req), nil
	case parts[0] == "secrets" && len(parts) >= 2:
		name := parts[1]
		lname := strings.ToLower(name)
		switch req.Method {
		case http.MethodPut:
			if _, gone := f.deleted[lname]; gone {
				return kvError(req, http.StatusConflict, "Conflict",
					fmt.Sprintf("Secret %s is currently in a deleted but recoverable state", name)), nil
			}
			var p struct {
				Value       string            `json:"value"`
				ContentType string            `json:"contentType"`
				Tags        map[string]string `json:"tags"`
			}
			if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
				return kvError(req, http.StatusBadRequest, "BadParameter", err.Error()), nil
			}
			s := f.secrets[lname]
			if s == nil {
				s = &fakeSecret{name: name}
				f.secrets[lname] = s
			}
			v := &fakeVersion{
				id:          fmt.Sprintf("ver%d", len(s.versions)+1),
				value:       p.Value,
				contentType: p.ContentType,
				tags:        p.Tags,
				created:     time.Unix(1700000000+int64(len(s.versions)), 0),
			}
			s.versions = append(s.versions, v)
			return jsonResp(req, http.StatusOK, f.bundle(s, v, true)), nil
		case http.MethodGet:
			s := f.secrets[lname]
			if s == nil {
				return kvError(req, http.StatusNotFound, "SecretNotFound", "A secret with (name/id) "+name+" was not found in this key vault."), nil
			}
			version := ""
			if len(parts) > 2 {
				version = parts[2]
			}
			if version == "" {
				return jsonResp(req, http.StatusOK, f.bundle(s, s.versions[len(s.versions)-1], true)), nil
			}
			for _, v := range s.versions {
				if v.id == version {
					return jsonResp(req, http.StatusOK, f.bundle(s, v, true)), nil
				}
			}
			return kvError(req, http.StatusNotFound, "SecretNotFound", "version not found"), nil
		case http.MethodDelete:
			s := f.secrets[lname]
			if s == nil {
				return kvError(req, http.StatusNotFound, "SecretNotFound", "not found"), nil
			}
			delete(f.secrets, lname)
			f.deleted[lname] = s
			b := f.bundle(s, s.versions[len(s.versions)-1], false)
			b["recoveryId"] = fakeVaultURL + "deletedsecrets/" + s.name
			return jsonResp(req, http.StatusOK, b), nil
		}
	case parts[0] == "deletedsecrets" && len(parts) >= 2:
		lname := strings.ToLower(parts[1])
		s := f.deleted[lname]
		if s == nil {
			return kvError(req, http.StatusNotFound, "SecretNotFound", "deleted secret not found"), nil
		}
		switch {
		case req.Method == http.MethodDelete && len(parts) == 2:
			delete(f.deleted, lname)
			return jsonResp(req, http.StatusNoContent, nil), nil
		case req.Method == http.MethodPost && len(parts) == 3 && parts[2] == "recover":
			delete(f.deleted, lname)
			f.secrets[lname] = s
			return jsonResp(req, http.StatusOK, f.bundle(s, s.versions[len(s.versions)-1], false)), nil
		}
	}
	return kvError(req, http.StatusBadRequest, "BadRequest", "unsupported "+req.Method+" "+req.URL.Path), nil
}

func (f *fakeVault) list(req *http.Request) *http.Response {
	names := make([]string, 0, len(f.secrets))
	for k := range f.secrets {
		names = append(names, k)
	}
	sort.Strings(names)
	start := 0
	if tok := req.URL.Query().Get("skip"); tok != "" {
		_, _ = fmt.Sscanf(tok, "%d", &start)
	}
	end := start + f.pageSize
	if end > len(names) {
		end = len(names)
	}
	items := make([]map[string]interface{}, 0, end-start)
	for _, n := range names[start:end] {
		s := f.secrets[n]
		b := f.bundle(s, s.versions[len(s.versions)-1], false)
		b["id"] = fakeVaultURL + "secrets/" + s.name
		items = append(items, b)
	}
	body := map[string]interface{}{"value": items}
	if end < len(names) {
		body["nextLink"] = fmt.Sprintf("%ssecrets?api-version=2025-07-01&skip=%d", fakeVaultURL, end)
	}
	return jsonResp(req, http.StatusOK, body)
}

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// newTestStore wires a KeyVaultSecretStore to a fake vault through the
// public constructor (azcfg credential + ClientOptions.Transport).
func newTestStore(t *testing.T, fv *fakeVault, cfg *KeyVaultSecretStoreConfig, opts ...Option) *KeyVaultSecretStore {
	t.Helper()
	if cfg == nil {
		cfg = &KeyVaultSecretStoreConfig{}
	}
	if cfg.VaultURL == "" && cfg.VaultName == "" {
		cfg.VaultURL = fakeVaultURL
	}
	cfg.Azure = testAzConfig(fv)
	s, err := NewKeyVaultSecretStore(context.Background(), cfg, opts...)
	if err != nil {
		t.Fatalf("NewKeyVaultSecretStore: %v", err)
	}
	return s
}

func testAzConfig(fv *fakeVault) *azcfg.Config {
	c := azcfg.NewConfig()
	c.SetCredential(fakeCred{})
	c.ClientOptions.Transport = fv
	c.ClientOptions.Retry.MaxRetries = -1
	return c
}
