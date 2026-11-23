package entra

import (
	"testing"

	"oss.nandlabs.io/golly/auth"
	"oss.nandlabs.io/golly/authz"
)

func TestClaimHelpers(t *testing.T) {
	c := &auth.Claims{Extra: map[string]any{
		"tid":         testTenant,
		"oid":         testOID,
		"ver":         "2.0",
		"appid":       "app-1",
		"unique_name": "bob@contoso.com",
		"scp":         " a  b ",
		"roles":       []any{"R1", 5, "", "R2"},
		"groups":      "single",
	}}
	if TenantID(c) != testTenant || ObjectID(c) != testOID || TokenVersion(c) != "2.0" {
		t.Error("string helpers")
	}
	if ClientAppID(c) != "app-1" || Username(c) != "bob@contoso.com" {
		t.Error("appid/unique_name")
	}
	if s := Scopes(c); len(s) != 2 || s[1] != "b" {
		t.Errorf("Scopes = %v", s)
	}
	if r := Roles(c); len(r) != 2 || r[1] != "R2" {
		t.Errorf("Roles = %v", r)
	}
	if g := Groups(c); len(g) != 1 || g[0] != "single" {
		t.Errorf("Groups = %v", g)
	}
	if HasGroupOverage(c) {
		t.Error("no overage expected")
	}
	if Username(&auth.Claims{}) != "" || TenantID(nil) != "" || Roles(nil) != nil || HasGroupOverage(nil) {
		t.Error("nil/empty handling")
	}
	c.Extra["groups"] = 42
	if Groups(c) != nil {
		t.Error("non-string groups")
	}
	c.Extra["groups"] = ""
	if Groups(c) != nil {
		t.Error("empty groups")
	}
	if !HasGroupOverage(&auth.Claims{Extra: map[string]any{"hasgroups": true}}) {
		t.Error("hasgroups")
	}
	if !HasGroupOverage(&auth.Claims{Extra: map[string]any{"_claim_names": map[string]any{"groups": "src1"}}}) {
		t.Error("_claim_names")
	}
}

func TestPrincipal(t *testing.T) {
	c := &auth.Claims{Extra: map[string]any{
		"tid": testTenant, "oid": testOID, "scp": "User.Read",
		"roles": []any{"Admin"}, "groups": []any{"g1", "g2"},
	}}
	p := NewPrincipal(c)
	var _ authz.Principal = p
	if p.ID() != testOID || p.ObjectID() != testOID || p.TenantID() != testTenant {
		t.Error("ids")
	}
	if len(p.Roles()) != 1 || len(p.Capabilities()) != 1 || len(p.Groups()) != 2 {
		t.Errorf("principal = %+v", p)
	}
	// Works with golly's RBAC.
	rbac := authz.NewRBAC()
	rbac.AddRole("Admin", "delete")
	if !rbac.Can(p, "delete", nil) {
		t.Error("rbac via app roles")
	}
	var nilP *Principal
	if nilP.ID() != "" || nilP.TenantID() != "" || nilP.Roles() != nil || nilP.Capabilities() != nil || nilP.Groups() != nil {
		t.Error("nil principal")
	}
}
