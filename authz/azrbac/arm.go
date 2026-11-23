package azrbac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"oss.nandlabs.io/golly-azure/azcfg"
)

// ErrUnsupportedScope is returned by ARMCaller.ListPermissions for
// scopes the Microsoft.Authorization/permissions API cannot be queried
// at through the SDK (anything other than a resource group or a
// resource).
var ErrUnsupportedScope = errors.New("azrbac: scope not supported by the permissions API")

const atScopeFilter = "atScope()"

// ARMCaller implements RBACCaller with the armauthorization/v2 SDK
// clients. It is safe for concurrent use.
type ARMCaller struct {
	assignments *armauthorization.RoleAssignmentsClient
	definitions *armauthorization.RoleDefinitionsClient
	denies      *armauthorization.DenyAssignmentsClient
	cred        azcore.TokenCredential
	opts        *arm.ClientOptions

	mu    sync.Mutex
	perms map[string]*armauthorization.PermissionsClient // per subscription
}

// NewARMCaller builds the role-assignment, role-definition,
// deny-assignment and permissions clients. subscriptionID may be empty:
// the scope-based list calls do not use it, and permissions clients are
// created per subscription parsed from the checked scope.
func NewARMCaller(subscriptionID string, cred azcore.TokenCredential, opts *arm.ClientOptions) (*ARMCaller, error) {
	if cred == nil {
		return nil, errors.New("azrbac: nil credential")
	}
	ra, err := armauthorization.NewRoleAssignmentsClient(subscriptionID, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azrbac: role assignments client: %w", err)
	}
	rd, err := armauthorization.NewRoleDefinitionsClient(cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azrbac: role definitions client: %w", err)
	}
	da, err := armauthorization.NewDenyAssignmentsClient(subscriptionID, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azrbac: deny assignments client: %w", err)
	}
	return &ARMCaller{
		assignments: ra,
		definitions: rd,
		denies:      da,
		cred:        cred,
		opts:        opts,
		perms:       map[string]*armauthorization.PermissionsClient{},
	}, nil
}

// NewARMCallerFromConfig builds an ARMCaller from an azcfg.Config
// (cfg.Credential(), cfg.SubscriptionID and cfg.ClientOpts()).
func NewARMCallerFromConfig(cfg *azcfg.Config) (*ARMCaller, error) {
	if cfg == nil {
		return nil, errors.New("azrbac: nil azcfg.Config")
	}
	cred, err := cfg.Credential()
	if err != nil {
		return nil, fmt.Errorf("azrbac: resolve credential: %w", err)
	}
	return NewARMCaller(cfg.SubscriptionID, cred, &arm.ClientOptions{ClientOptions: cfg.ClientOpts()})
}

// sdkScope strips the leading slash: the SDK templates "/{scope}/...".
func sdkScope(scope string) string { return strings.TrimPrefix(scope, "/") }

// ListRoleAssignments implements RBACCaller using
// RoleAssignmentsClient.NewListForScopePager with $filter=atScope().
func (c *ARMCaller) ListRoleAssignments(ctx context.Context, scope string) ([]RoleAssignment, error) {
	filter := atScopeFilter
	pager := c.assignments.NewListForScopePager(sdkScope(scope), &armauthorization.RoleAssignmentsClientListForScopeOptions{Filter: &filter})
	var out []RoleAssignment
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, ra := range page.Value {
			if ra == nil || ra.Properties == nil {
				continue
			}
			p := ra.Properties
			a := RoleAssignment{
				ID:               deref(ra.ID),
				Scope:            deref(p.Scope),
				PrincipalID:      deref(p.PrincipalID),
				RoleDefinitionID: deref(p.RoleDefinitionID),
				Condition:        deref(p.Condition),
			}
			if p.PrincipalType != nil {
				a.PrincipalType = string(*p.PrincipalType)
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// GetRoleDefinition implements RBACCaller using RoleDefinitionsClient.GetByID.
func (c *ARMCaller) GetRoleDefinition(ctx context.Context, id string) (RoleDefinition, error) {
	resp, err := c.definitions.GetByID(ctx, sdkScope(id), nil)
	if err != nil {
		return RoleDefinition{}, err
	}
	def := RoleDefinition{ID: deref(resp.ID)}
	if resp.Properties != nil {
		def.RoleName = deref(resp.Properties.RoleName)
		def.Permissions = convertPermissions(resp.Properties.Permissions)
	}
	return def, nil
}

// ListDenyAssignments implements RBACCaller using
// DenyAssignmentsClient.NewListForScopePager with $filter=atScope().
// Conditional deny permissions are kept (applied unconditionally) so
// the evaluator errs on the side of denying.
func (c *ARMCaller) ListDenyAssignments(ctx context.Context, scope string) ([]DenyAssignment, error) {
	filter := atScopeFilter
	pager := c.denies.NewListForScopePager(sdkScope(scope), &armauthorization.DenyAssignmentsClientListForScopeOptions{Filter: &filter})
	var out []DenyAssignment
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, da := range page.Value {
			if da == nil || da.Properties == nil {
				continue
			}
			p := da.Properties
			d := DenyAssignment{
				ID:    deref(da.ID),
				Scope: deref(p.Scope),
			}
			if p.DoNotApplyToChildScopes != nil {
				d.DoNotApplyToChildScopes = *p.DoNotApplyToChildScopes
			}
			for _, perm := range p.Permissions {
				if perm == nil {
					continue
				}
				d.Permissions = append(d.Permissions, Permission{
					Actions:        derefAll(perm.Actions),
					NotActions:     derefAll(perm.NotActions),
					DataActions:    derefAll(perm.DataActions),
					NotDataActions: derefAll(perm.NotDataActions),
				})
			}
			for _, pr := range p.Principals {
				if pr != nil && pr.ID != nil {
					d.Principals = append(d.Principals, *pr.ID)
				}
			}
			for _, pr := range p.ExcludePrincipals {
				if pr != nil && pr.ID != nil {
					d.ExcludePrincipals = append(d.ExcludePrincipals, *pr.ID)
				}
			}
			out = append(out, d)
		}
	}
	return out, nil
}

// ListPermissions implements RBACCaller using PermissionsClient's
// ListForResourceGroup / ListForResource pagers. The scope must be a
// resource group (/subscriptions/<s>/resourceGroups/<rg>) or a resource
// below one; other scopes return ErrUnsupportedScope.
func (c *ARMCaller) ListPermissions(ctx context.Context, scope string) ([]Permission, error) {
	ps, err := parseScope(scope)
	if err != nil {
		return nil, err
	}
	client, err := c.permissionsClient(ps.subscription)
	if err != nil {
		return nil, err
	}
	var out []Permission
	if ps.resourceName == "" {
		pager := client.NewListForResourceGroupPager(ps.resourceGroup, nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, convertPermissions(page.Value)...)
		}
		return out, nil
	}
	pager := client.NewListForResourcePager(ps.resourceGroup, ps.namespace, ps.parentPath, ps.resourceType, ps.resourceName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, convertPermissions(page.Value)...)
	}
	return out, nil
}

func (c *ARMCaller) permissionsClient(sub string) (*armauthorization.PermissionsClient, error) {
	key := strings.ToLower(sub)
	c.mu.Lock()
	defer c.mu.Unlock()
	if pc, ok := c.perms[key]; ok {
		return pc, nil
	}
	pc, err := armauthorization.NewPermissionsClient(sub, c.cred, c.opts)
	if err != nil {
		return nil, fmt.Errorf("azrbac: permissions client: %w", err)
	}
	c.perms[key] = pc
	return pc, nil
}

// parsedScope is an ARM scope decomposed for the permissions API.
type parsedScope struct {
	subscription  string
	resourceGroup string
	namespace     string
	parentPath    string
	resourceType  string
	resourceName  string
}

// parseScope decomposes
// /subscriptions/<s>/resourceGroups/<rg>[/providers/<ns>/<type>/<name>[/<childType>/<childName>...]].
func parseScope(scope string) (parsedScope, error) {
	var ps parsedScope
	parts := strings.Split(strings.Trim(scope, "/"), "/")
	if len(parts) < 4 || !strings.EqualFold(parts[0], "subscriptions") || !strings.EqualFold(parts[2], "resourceGroups") ||
		parts[1] == "" || parts[3] == "" {
		return ps, fmt.Errorf("%w: %q (need a resource group or resource scope)", ErrUnsupportedScope, scope)
	}
	ps.subscription, ps.resourceGroup = parts[1], parts[3]
	rest := parts[4:]
	if len(rest) == 0 {
		return ps, nil
	}
	// providers/<ns>/<type>/<name>[/<type>/<name>]*
	if len(rest) < 4 || !strings.EqualFold(rest[0], "providers") || len(rest[2:])%2 != 0 {
		return ps, fmt.Errorf("%w: %q (malformed resource id)", ErrUnsupportedScope, scope)
	}
	for _, seg := range rest {
		if seg == "" {
			return ps, fmt.Errorf("%w: %q (empty path segment)", ErrUnsupportedScope, scope)
		}
	}
	ps.namespace = rest[1]
	pairs := rest[2:]
	n := len(pairs)
	ps.resourceType, ps.resourceName = pairs[n-2], pairs[n-1]
	ps.parentPath = strings.Join(pairs[:n-2], "/")
	return ps, nil
}

func convertPermissions(in []*armauthorization.Permission) []Permission {
	out := make([]Permission, 0, len(in))
	for _, p := range in {
		if p == nil {
			continue
		}
		out = append(out, Permission{
			Actions:        derefAll(p.Actions),
			NotActions:     derefAll(p.NotActions),
			DataActions:    derefAll(p.DataActions),
			NotDataActions: derefAll(p.NotDataActions),
		})
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefAll(in []*string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != nil && *s != "" {
			out = append(out, *s)
		}
	}
	return out
}
