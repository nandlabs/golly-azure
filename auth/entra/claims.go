package entra

import (
	"strings"

	"oss.nandlabs.io/golly/auth"
)

// TenantID returns the tid claim (directory id) of verified claims.
func TenantID(c *auth.Claims) string { return claimString(c, "tid") }

// ObjectID returns the oid claim: the immutable object id of the user
// or service principal in the tenant. Prefer it over sub (which is
// pairwise per application) when keying users or Azure RBAC lookups.
func ObjectID(c *auth.Claims) string { return claimString(c, "oid") }

// TokenVersion returns the ver claim ("1.0" or "2.0").
func TokenVersion(c *auth.Claims) string { return claimString(c, "ver") }

// ClientAppID returns the id of the client application that requested
// the token: azp on v2.0 tokens, appid on v1.0 tokens.
func ClientAppID(c *auth.Claims) string {
	if s := claimString(c, "azp"); s != "" {
		return s
	}
	return claimString(c, "appid")
}

// Username returns a human-readable account name: preferred_username
// (v2.0), then upn, then unique_name (v1.0). It is NOT a stable
// identifier — use ObjectID for that.
func Username(c *auth.Claims) string {
	for _, k := range []string{"preferred_username", "upn", "unique_name"} {
		if s := claimString(c, k); s != "" {
			return s
		}
	}
	return ""
}

// Scopes returns the delegated permissions in the space-separated scp
// claim (empty for app-only tokens).
func Scopes(c *auth.Claims) []string {
	return strings.Fields(claimString(c, "scp"))
}

// Roles returns the app roles in the roles claim (app roles assigned to
// the user, or application permissions for app-only tokens).
func Roles(c *auth.Claims) []string { return claimStrings(c, "roles") }

// Groups returns the group object ids in the groups claim. Entra emits
// this claim only when the app registration's groupMembershipClaims is
// configured, and omits it (setting _claim_names/hasgroups instead) when
// the user is in too many groups — see HasGroupOverage.
func Groups(c *auth.Claims) []string { return claimStrings(c, "groups") }

// HasGroupOverage reports whether Entra omitted the groups claim
// because the user belongs to more groups than fit in a token. Callers
// must then resolve membership through Microsoft Graph.
func HasGroupOverage(c *auth.Claims) bool {
	if c == nil || c.Extra == nil {
		return false
	}
	if b, ok := c.Extra["hasgroups"].(bool); ok && b {
		return true
	}
	if names, ok := c.Extra["_claim_names"].(map[string]any); ok {
		if _, ok := names["groups"]; ok {
			return true
		}
	}
	return false
}

// Principal is an authz.Principal built from verified Entra claims. It
// exposes the object id through ID()/ObjectID() and the token's group
// ids through Groups(), which is what the golly-azure authz/azrbac
// evaluator consumes to expand group-based role assignments.
type Principal struct {
	ObjectIDValue string
	TenantIDValue string
	RoleList      []string
	ScopeList     []string
	GroupList     []string
}

// NewPrincipal builds a Principal from verified claims. Roles() returns
// the roles claim and Capabilities() the scp scopes.
func NewPrincipal(c *auth.Claims) *Principal {
	return &Principal{
		ObjectIDValue: ObjectID(c),
		TenantIDValue: TenantID(c),
		RoleList:      Roles(c),
		ScopeList:     Scopes(c),
		GroupList:     Groups(c),
	}
}

// ID returns the principal's object id.
func (p *Principal) ID() string {
	if p == nil {
		return ""
	}
	return p.ObjectIDValue
}

// ObjectID returns the principal's object id.
func (p *Principal) ObjectID() string { return p.ID() }

// TenantID returns the principal's tenant id.
func (p *Principal) TenantID() string {
	if p == nil {
		return ""
	}
	return p.TenantIDValue
}

// Roles implements authz.Principal (Entra app roles).
func (p *Principal) Roles() []string {
	if p == nil {
		return nil
	}
	return p.RoleList
}

// Capabilities implements authz.Principal (delegated scopes).
func (p *Principal) Capabilities() []string {
	if p == nil {
		return nil
	}
	return p.ScopeList
}

// Groups returns the group object ids carried in the token.
func (p *Principal) Groups() []string {
	if p == nil {
		return nil
	}
	return p.GroupList
}

func claimString(c *auth.Claims, key string) string {
	if c == nil {
		return ""
	}
	return stringClaim(c.Extra, key)
}

func stringClaim(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func claimStrings(c *auth.Claims, key string) []string {
	if c == nil || c.Extra == nil {
		return nil
	}
	switch v := c.Extra[key].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}
