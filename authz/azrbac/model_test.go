package azrbac

import "testing"

func TestWildcardMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, action string
		want            bool
	}{
		{"*", "Microsoft.Compute/virtualMachines/read", true},
		{"*/read", "Microsoft.Compute/virtualMachines/read", true},
		{"*/read", "Microsoft.Compute/virtualMachines/write", false},
		{"Microsoft.Compute/*", "microsoft.compute/virtualMachines/start/action", true},
		{"Microsoft.Compute/*", "Microsoft.ComputeX/foo", false},
		{"Microsoft.Authorization/*/Delete", "Microsoft.Authorization/roleAssignments/delete", true},
		{"Microsoft.Authorization/*/Delete", "Microsoft.Authorization/roleAssignments/write", false},
		{"Microsoft.Storage/*/blobs/*", "Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"exact", "EXACT", true},
		{"exact", "exactly", false},
		{"**", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.action); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.action, got, c.want)
		}
	}
}

func TestNormalizeScopeAndContains(t *testing.T) {
	t.Parallel()
	if got := normalizeScope(" /Subscriptions/S/ResourceGroups/RG// "); got != "/subscriptions/s/resourcegroups/rg" {
		t.Errorf("normalizeScope = %q", got)
	}
	if normalizeScope("/") != "/" || normalizeScope("subscriptions/x") != "/subscriptions/x" || normalizeScope("") != "" {
		t.Error("normalizeScope edge cases")
	}
	if !scopeContains("/", "/anything") || !scopeContains("/a", "/a") || !scopeContains("/a", "/a/b") || scopeContains("/a", "/ab") {
		t.Error("scopeContains")
	}
}

func TestAllows(t *testing.T) {
	t.Parallel()
	perms := []Permission{
		{Actions: []string{"*"}, NotActions: []string{"Microsoft.Authorization/*/write"}},
		{DataActions: []string{"Microsoft.Storage/*"}, NotDataActions: []string{"*/delete"}},
	}
	if !allows(perms, "Microsoft.Web/sites/write", false) || allows(perms, "Microsoft.Authorization/roleAssignments/write", false) {
		t.Error("control plane")
	}
	if !allows(perms, "Microsoft.Storage/x/read", true) || allows(perms, "Microsoft.Storage/x/delete", true) || allows(perms, "Microsoft.Web/x", true) {
		t.Error("data plane")
	}
	// NotActions in one block don't subtract from another block.
	split := []Permission{{Actions: []string{"a/*"}, NotActions: []string{"a/b"}}, {Actions: []string{"a/b"}}}
	if !allows(split, "a/b", false) {
		t.Error("NotActions must be per-block")
	}
	if DataAction("x") != "data:x" {
		t.Error("DataAction")
	}
}
