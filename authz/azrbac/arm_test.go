package azrbac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"oss.nandlabs.io/golly-azure/azcfg"
)

type staticCred struct{}

func (staticCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// armFake is a policy.Transporter that emulates the handful of
// Microsoft.Authorization endpoints ARMCaller uses.
type armFake struct {
	mu       sync.Mutex
	requests []*http.Request
	routes   map[string]any // path (lower-case) -> JSON body
	status   int
}

func (f *armFake) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if req.Header.Get("Authorization") != "Bearer test-token" {
		return respond(req, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "Unauthorized"}}), nil
	}
	if f.status != 0 {
		return respond(req, f.status, map[string]any{"error": map[string]string{"code": "InternalServerError", "message": "boom"}}), nil
	}
	key := strings.ToLower(req.URL.Path)
	if p := req.URL.Query().Get("page"); p != "" {
		key += "?page=" + p
	}
	body, ok := f.routes[key]
	if !ok {
		return respond(req, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "NotFound", "message": key}}), nil
	}
	return respond(req, http.StatusOK, body), nil
}

func respond(req *http.Request, status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Request:    req,
	}
}

func newARMFake() *armFake {
	const host = "https://management.azure.com"
	rgPath := strings.ToLower(rg)
	return &armFake{routes: map[string]any{
		rgPath + "/providers/microsoft.authorization/roleassignments": map[string]any{
			"value": []any{
				map[string]any{"id": rg + "/providers/Microsoft.Authorization/roleAssignments/ra1", "properties": map[string]any{
					"scope": rg, "principalId": alice, "principalType": "User", "roleDefinitionId": readerID,
				}},
				map[string]any{"id": "nil-props"},
			},
			"nextLink": host + rg + "/providers/Microsoft.Authorization/roleAssignments?page=2",
		},
		rgPath + "/providers/microsoft.authorization/roleassignments?page=2": map[string]any{
			"value": []any{
				map[string]any{"id": "ra2", "properties": map[string]any{
					"scope": sub, "principalId": devGroup, "principalType": "Group", "roleDefinitionId": contributorID,
					"condition": "",
				}},
			},
		},
		strings.ToLower(readerID): map[string]any{"id": readerID, "properties": map[string]any{
			"roleName": "Reader", "permissions": []any{map[string]any{"actions": []string{"*/read"}}},
		}},
		strings.ToLower(contributorID): map[string]any{"id": contributorID, "properties": map[string]any{
			"roleName": "Contributor", "permissions": []any{map[string]any{
				"actions": []string{"*"}, "notActions": []string{"Microsoft.Authorization/*/Write"},
			}},
		}},
		rgPath + "/providers/microsoft.authorization/denyassignments": map[string]any{
			"value": []any{
				map[string]any{"id": "da1", "properties": map[string]any{
					"scope": sub, "doNotApplyToChildScopes": false,
					"permissions":       []any{map[string]any{"actions": []string{"Microsoft.Network/*/delete"}}},
					"principals":        []any{map[string]any{"id": EveryonePrincipalID, "type": "SystemDefined"}},
					"excludePrincipals": []any{map[string]any{"id": bob, "type": "User"}},
				}},
				map[string]any{"id": "da-nil"},
			},
		},
		rgPath + "/providers/microsoft.authorization/permissions": map[string]any{
			"value": []any{map[string]any{"actions": []string{"*/read"}, "notActions": []string{}}},
		},
		rgPath + "/providers/microsoft.compute/virtualmachines/vm1/providers/microsoft.authorization/permissions": map[string]any{
			"value": []any{map[string]any{"actions": []string{"Microsoft.Compute/virtualMachines/*"}}},
		},
		rgPath + "/providers/microsoft.sql/servers/srv/databases/db1/providers/microsoft.authorization/permissions": map[string]any{
			"value": []any{map[string]any{"actions": []string{"Microsoft.Sql/servers/databases/*"}}},
		},
	}}
}

func newTestARMCaller(t *testing.T, f *armFake) *ARMCaller {
	t.Helper()
	c, err := NewARMCaller("00000000-1111-2222-3333-444444444444", staticCred{}, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Transport: f, Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestARMCaller_EndToEndWithEvaluator(t *testing.T) {
	f := newARMFake()
	c := newTestARMCaller(t, f)
	e := NewCachedEvaluator(c, rg, 0)
	defer func() { _ = e.Close() }()
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	a := &objPrincipal{oid: alice}
	if !e.Can(a, "Microsoft.Web/sites/read", nil) || e.Can(a, "Microsoft.Web/sites/write", nil) {
		t.Error("reader via ARM")
	}
	member := &objPrincipal{oid: bob, groups: []string{devGroup}}
	if !e.Can(member, "Microsoft.Web/sites/write", nil) {
		t.Error("group contributor (page 2) via ARM")
	}
	if !e.Can(member, "Microsoft.Network/virtualNetworks/delete", nil) {
		t.Error("bob excluded from deny")
	}
	if e.Can(&objPrincipal{oid: "x", groups: []string{devGroup}}, "Microsoft.Network/virtualNetworks/delete", nil) {
		t.Error("everyone deny via ARM")
	}
	// Verify the atScope() filter and leading-slash handling.
	sawFilter := false
	for _, r := range f.requests {
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("double slash in %s", r.URL.Path)
		}
		if strings.Contains(r.URL.RawQuery, "$filter=atScope()") {
			sawFilter = true
		}
	}
	if !sawFilter {
		t.Error("atScope() filter not sent")
	}

	// Live checks through the permissions API.
	ctx := context.Background()
	if !e.Can(a, "Microsoft.Web/sites/read", RBACResource{Ctx: ctx, Scope: rg}) {
		t.Error("live RG")
	}
	if !e.Can(a, "Microsoft.Compute/virtualMachines/start/action", RBACResource{Ctx: ctx, Scope: vm}) {
		t.Error("live resource")
	}
	db := rg + "/providers/Microsoft.Sql/servers/srv/databases/db1"
	if !e.Can(a, "Microsoft.Sql/servers/databases/read", RBACResource{Ctx: ctx, Scope: db}) {
		t.Error("live nested resource")
	}
	if e.Can(a, "Microsoft.Web/sites/read", RBACResource{Ctx: ctx, Scope: sub}) {
		t.Error("live subscription scope is unsupported -> false")
	}
}

func TestARMCaller_Errors(t *testing.T) {
	f := newARMFake()
	f.status = http.StatusInternalServerError
	c := newTestARMCaller(t, f)
	ctx := context.Background()
	if _, err := c.ListRoleAssignments(ctx, rg); err == nil {
		t.Error("ListRoleAssignments")
	}
	if _, err := c.GetRoleDefinition(ctx, readerID); err == nil {
		t.Error("GetRoleDefinition")
	}
	if _, err := c.ListDenyAssignments(ctx, rg); err == nil {
		t.Error("ListDenyAssignments")
	}
	if _, err := c.ListPermissions(ctx, rg); err == nil {
		t.Error("ListPermissions RG")
	}
	if _, err := c.ListPermissions(ctx, vm); err == nil {
		t.Error("ListPermissions resource")
	}
	if _, err := c.ListPermissions(ctx, sub); !errors.Is(err, ErrUnsupportedScope) {
		t.Errorf("subscription scope: %v", err)
	}
	if _, err := NewARMCaller("s", nil, nil); err == nil {
		t.Error("nil credential")
	}
	if _, err := NewARMCallerFromConfig(nil); err == nil {
		t.Error("nil config")
	}
	cfg := azcfg.NewConfig()
	cfg.SetSubscriptionID("s")
	cfg.SetCredential(staticCred{})
	if c, err := NewARMCallerFromConfig(cfg); err != nil || c == nil {
		t.Errorf("from config: %v", err)
	}
	bad := azcfg.NewConfig()
	bad.ClientSecret = "x" // missing tenant/client id
	if _, err := NewARMCallerFromConfig(bad); err == nil {
		t.Error("bad credential config")
	}
}

func TestParseScope(t *testing.T) {
	t.Parallel()
	ok := map[string]parsedScope{
		rg: {subscription: "00000000-1111-2222-3333-444444444444", resourceGroup: "rg-app"},
		vm: {subscription: "00000000-1111-2222-3333-444444444444", resourceGroup: "rg-app", namespace: "Microsoft.Compute", resourceType: "virtualMachines", resourceName: "vm1"},
		rg + "/providers/Microsoft.Sql/servers/s/databases/d/": {subscription: "00000000-1111-2222-3333-444444444444", resourceGroup: "rg-app", namespace: "Microsoft.Sql", parentPath: "servers/s", resourceType: "databases", resourceName: "d"},
	}
	for in, want := range ok {
		got, err := parseScope(in)
		if err != nil || got != want {
			t.Errorf("parseScope(%q) = %+v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "/", sub, mgScope, sub + "/resourceGroups/", rg + "/foo", rg + "/providers/ns/type", rg + "/providers/ns/t/n/x", rg + "/providers/ns//n"} {
		if _, err := parseScope(in); !errors.Is(err, ErrUnsupportedScope) {
			t.Errorf("parseScope(%q) err = %v", in, err)
		}
	}
}
