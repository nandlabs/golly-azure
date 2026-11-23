// Package azrbac adapts Azure role-based access control (Azure RBAC) to
// the oss.nandlabs.io/golly/authz.Policy interface.
//
// The upstream Policy interface has the signature
//
//	Can(p Principal, action string, resource any) bool
//
// which is context-free and returns a plain bool, while Azure RBAC data
// lives behind Azure Resource Manager calls that need a context and can
// fail. Unlike AWS (iam:SimulatePrincipalPolicy) and GCP
// (TestIamPermissions), Azure has no general "simulate this principal"
// API, so this package offers two shapes:
//
//   - A cached Evaluator that reproduces the RBAC decision locally. A
//     background goroutine (or manual Refresh) lists every role
//     assignment at or above the configured scope ($filter=atScope(),
//     all principals), resolves each role definition's Actions,
//     NotActions, DataActions and NotDataActions (cached with a TTL), and
//     lists deny assignments at or above the scope. Can then grants iff
//     an unconditional assignment to the principal's object id — or to
//     one of its group object ids — grants the action (wildcards,
//     case-insensitive, NotActions subtracted per role) at the target
//     scope, and no deny assignment blocks it. This is the recommended
//     production shape.
//
//   - An RBACResource{Ctx, Scope} envelope callers pass as the `resource
//     any` argument for a live Microsoft.Authorization/permissions call.
//     That API reports the effective permissions of the identity the ARM
//     client authenticates as (not of an arbitrary principal), so pair it
//     with WithExpectedMember.
//
// Actions are control-plane by default; tag data-plane actions with
// DataAction (the "data:" prefix) or register them via WithDataActions.
//
// Limitations (all fail closed unless noted):
//   - ABAC conditions on role assignments are not evaluated; conditional
//     assignments are ignored.
//   - Conditions on deny assignments are not evaluated; such denies are
//     applied unconditionally.
//   - Group membership is not expanded through Microsoft Graph: group
//     ids come from the Principal (interface{ Groups() []string }, e.g.
//     golly-azure auth/entra.Principal built from the token's groups
//     claim, which already contains transitive security-group
//     membership) or from WithPrincipalGroups. Tokens with group overage
//     carry no groups claim.
//   - Only assignments at or above the base scope are cached. Checking a
//     scope below it ignores assignments made below the base scope
//     (under-grant) and deny assignments made below it (may over-grant);
//     set the base scope to the most specific scope you check, or use the
//     live path. Management-group assignments are honoured for the base
//     scope and its descendants only.
//   - Classic (co-)administrators, PIM-eligible (not yet activated)
//     assignments and Entra directory roles are not considered.
package azrbac
