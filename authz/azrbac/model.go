package azrbac

import (
	"context"
	"strings"
)

// Permission is one permission block of an Azure role definition (or
// of a caller's effective permissions / a deny assignment). Entries may
// contain '*' wildcards and are matched case-insensitively.
type Permission struct {
	Actions        []string
	NotActions     []string
	DataActions    []string
	NotDataActions []string
}

// RoleAssignment is the subset of a Microsoft.Authorization role
// assignment the evaluator needs.
type RoleAssignment struct {
	ID               string
	Scope            string
	PrincipalID      string
	PrincipalType    string
	RoleDefinitionID string
	// Condition is the ABAC condition expression, if any. Conditional
	// assignments are ignored by the cached evaluator (fail closed).
	Condition string
}

// RoleDefinition is the subset of a Microsoft.Authorization role
// definition the evaluator needs.
type RoleDefinition struct {
	ID          string
	RoleName    string
	Permissions []Permission
}

// DenyAssignment is the subset of a Microsoft.Authorization deny
// assignment the evaluator needs. Principals / ExcludePrincipals hold
// object ids; EveryonePrincipalID in Principals means "all principals".
type DenyAssignment struct {
	ID                      string
	Scope                   string
	Permissions             []Permission
	Principals              []string
	ExcludePrincipals       []string
	DoNotApplyToChildScopes bool
}

// EveryonePrincipalID is the system-defined principal id Azure uses in
// deny assignments to mean "every principal".
const EveryonePrincipalID = "00000000-0000-0000-0000-000000000000"

// RBACCaller is the minimal surface the evaluator uses on Azure
// Resource Manager. *ARMCaller implements it on top of the
// armauthorization/v2 SDK clients; tests inject a fake.
type RBACCaller interface {
	// ListRoleAssignments returns every role assignment that applies at
	// scope — i.e. assignments at or above it ($filter=atScope()), for
	// all principals.
	ListRoleAssignments(ctx context.Context, scope string) ([]RoleAssignment, error)
	// GetRoleDefinition resolves a role definition by its full
	// resource id.
	GetRoleDefinition(ctx context.Context, id string) (RoleDefinition, error)
	// ListDenyAssignments returns every deny assignment at or above
	// scope ($filter=atScope()).
	ListDenyAssignments(ctx context.Context, scope string) ([]DenyAssignment, error)
	// ListPermissions returns the effective permissions of the
	// *calling identity* (the credential the client authenticates
	// with) at scope, via Microsoft.Authorization/permissions.
	ListPermissions(ctx context.Context, scope string) ([]Permission, error)
}

// DataActionPrefix marks an action passed to Evaluator.Can as a data-
// plane action (matched against DataActions/NotDataActions) rather than
// a control-plane action (Actions/NotActions). See DataAction and
// WithDataActions.
const DataActionPrefix = "data:"

// DataAction returns action tagged as a data-plane action, e.g.
//
//	eval.Can(p, azrbac.DataAction("Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read"), nil)
func DataAction(action string) string { return DataActionPrefix + action }

// allows reports whether any permission block grants action. Within a
// block, NotActions only subtract from that block's Actions — exactly
// how Azure combines multiple role assignments.
func allows(perms []Permission, action string, data bool) bool {
	for _, p := range perms {
		inc, exc := p.Actions, p.NotActions
		if data {
			inc, exc = p.DataActions, p.NotDataActions
		}
		if matchAny(inc, action) && !matchAny(exc, action) {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, action string) bool {
	for _, p := range patterns {
		if wildcardMatch(p, action) {
			return true
		}
	}
	return false
}

// wildcardMatch matches an Azure RBAC action pattern against an action
// case-insensitively. '*' matches any (possibly empty) run of
// characters, including '/'.
func wildcardMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	p := strings.ToLower(pattern)
	s = strings.ToLower(s)
	// Iterative glob with single-star backtracking.
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// normalizeScope lower-cases a scope and strips trailing slashes so
// scopes compare as ARM does (case-insensitively). The root scope "/"
// is preserved.
func normalizeScope(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	if s != "" && !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return s
}

// scopeContains reports whether child is equal to or below parent.
func scopeContains(parent, child string) bool {
	if parent == "/" {
		return true
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}
