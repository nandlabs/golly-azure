# azrbac — Azure RBAC authorizer for `golly/authz`

`azrbac` adapts Azure role-based access control to the
`oss.nandlabs.io/golly/authz.Policy` interface, so role assignments in
Azure Resource Manager can be the source of truth for
`Can(principal, action, resource)` checks in a Golly app.

## Why this package looks the way it does

`authz.Policy.Can(p Principal, action string, resource any) bool` is
deliberately context-free and returns a plain bool. Azure RBAC lives
behind ARM calls that need a `context.Context` and can fail — and,
unlike AWS (`iam:SimulatePrincipalPolicy`) or GCP
(`TestIamPermissions`), Azure has **no general "simulate a principal"
API**. This package offers two shapes:

- **Cached evaluator (recommended)** — a background goroutine refreshes
  a snapshot of every role assignment at or above a base scope
  (`$filter=atScope()`, all principals), the referenced role
  definitions (`Actions` / `NotActions` / `DataActions` /
  `NotDataActions`, cached with a TTL) and the deny assignments at or
  above the scope. `Can` evaluates locally: wildcard + case-insensitive
  action matching, `NotActions` subtracted per role, scope inheritance,
  group assignments, deny assignments with `Everyone`, exclusions and
  `DoNotApplyToChildScopes`.
- **Live check via `RBACResource`** — pass `RBACResource{Ctx, Scope}` as
  the `resource any` argument. `Can` calls
  `Microsoft.Authorization/permissions` for that scope, which returns the
  effective permissions of **the identity the ARM client is authenticated
  as** — pin it with `WithExpectedMember(<that object id>)`.

## Usage

```go
import (
    "context"
    "time"

    "oss.nandlabs.io/golly-azure/auth/entra"
    "oss.nandlabs.io/golly-azure/authz/azrbac"
    "oss.nandlabs.io/golly-azure/azcfg"
    "oss.nandlabs.io/golly/authz"
)

func main() {
    ctx := context.Background()

    // 1. ARM caller — credential from azcfg (DefaultAzureCredential by default).
    cfg := azcfg.NewConfig()
    cfg.SetSubscriptionID("00000000-1111-2222-3333-444444444444")
    caller, err := azrbac.NewARMCallerFromConfig(cfg)
    if err != nil { /* handle */ }

    rg := "/subscriptions/00000000-1111-2222-3333-444444444444/resourceGroups/rg-app"

    // 2. Cached evaluator for the resource group, refreshed every minute.
    eval := azrbac.NewCachedEvaluator(caller, rg, time.Minute,
        azrbac.WithMaxStaleness(10*time.Minute)) // fail closed if ARM is unreachable
    defer eval.Close()
    if err := eval.Refresh(ctx); err != nil { /* fail fast */ }

    // 3. Principal from a verified Entra token: ID() = oid, Groups() = groups claim.
    claims, _ := verifier.VerifyToken(ctx, bearer)
    p := entra.NewPrincipal(claims)

    // 4. Control-plane action at the base scope...
    if eval.Can(p, "Microsoft.Compute/virtualMachines/start/action", nil) { /* ... */ }
    // ...at a child scope...
    vm := rg + "/providers/Microsoft.Compute/virtualMachines/vm1"
    if eval.Can(p, "Microsoft.Compute/virtualMachines/restart/action", vm) { /* ... */ }
    // ...or a data-plane action.
    if eval.Can(p, azrbac.DataAction("Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read"), nil) { /* ... */ }

    // 5. Compose with any other Policy.
    rbac := authz.NewRBAC()
    rbac.AddRole("Admin", "Microsoft.Compute/virtualMachines/start/action") // Entra app role
    policy := authz.Any(eval, rbac)
    _ = policy

    // 6. Live check of the app's own permissions on a specific resource.
    self := azrbac.NewCachedEvaluator(caller, rg, 0, azrbac.WithExpectedMember(appObjectID))
    if self.Can(appPrincipal, "Microsoft.Storage/storageAccounts/listKeys/action",
        azrbac.RBACResource{Ctx: ctx, Scope: rg + "/providers/Microsoft.Storage/storageAccounts/acct"}) {
        // proceed
    }
}
```

Any `authz.Principal` works as long as its object id is reachable: the
default extractor tries `ObjectID()`, `ID()` and `Member()` accessors,
and group ids come from `Groups() []string`. Override with
`WithPrincipalMember` / `WithPrincipalGroups`. `Evaluate(objectID,
groupIDs, scope, action)` is the same check without a Principal.

## Options

| Option                         | Effect                                                                                   |
|--------------------------------|------------------------------------------------------------------------------------------|
| `WithPrincipalMember(fn)`      | Map a Principal to its Entra object id.                                                  |
| `WithPrincipalGroups(fn)`      | Map a Principal to its group object ids (assignments to those groups apply).             |
| `WithExpectedMember(oid)`      | Deny any Principal whose object id isn't `oid` (recommended with live checks).           |
| `WithDataActions(a...)`        | Treat these actions as data actions without the `data:` prefix.                          |
| `WithoutDenyAssignments()`     | Don't list/apply deny assignments (identity lacks `denyAssignments/read`).               |
| `WithRoleDefinitionTTL(d)`     | Reuse resolved role definitions across refreshes for `d` (default 1h).                   |
| `WithMaxStaleness(d)`          | Deny everything if the last successful refresh is older than `d` (default: never stale).|
| `WithClock(fn)`                | Time source override (tests).                                                            |

## Behaviour

- `Can(p, action, resource)`:
  - Denies when the principal has no object id, or `WithExpectedMember`
    is set and doesn't match.
  - `RBACResource{Ctx, Scope}` / `*RBACResource` → live
    `Microsoft.Authorization/permissions` for `Scope` (or the base scope
    when empty). Only resource-group and resource scopes are supported by
    that API; other scopes and errors → `false`.
  - A `string` starting with `/` → cached evaluation at that ARM scope.
  - Anything else (including `nil`) → cached evaluation at the base scope.
  - Before the first successful `Refresh`, everything is denied.
- `Refresh(ctx)` lists role assignments (`atScope()`), resolves role
  definitions (`GetByID`, TTL-cached), lists deny assignments
  (`atScope()`), then atomically swaps the snapshot. On error the
  previous snapshot is kept.
- `Close` is idempotent and waits for the refresh goroutine to exit.

## Permissions required

The ARM identity needs `Microsoft.Authorization/roleAssignments/read`,
`Microsoft.Authorization/roleDefinitions/read` and
`Microsoft.Authorization/denyAssignments/read` on the base scope — the
built-in **Reader** role is sufficient.

## Limitations

- **ABAC conditions** on role assignments are not evaluated — conditional
  assignments are ignored (fail closed). Conditions on deny assignments
  are not evaluated either — those denies apply unconditionally.
- **Group membership is not expanded** via Microsoft Graph. Group ids
  must come from the Principal — e.g. `entra.NewPrincipal(claims)` uses
  the token's `groups` claim (which already lists transitive
  security-group membership when `groupMembershipClaims` is configured).
  Tokens with group overage (`entra.HasGroupOverage`) carry no groups;
  resolve them yourself and supply them via `WithPrincipalGroups`.
- Only assignments **at or above the base scope** are cached. Checking a
  child scope ignores role assignments made below the base scope
  (under-grant) and deny assignments made below it (possible
  over-grant). Use the most specific scope you check as the base scope,
  or one evaluator per scope. Management-group assignments are honoured
  for the base scope and its descendants; for scopes outside the base
  they are conservatively ignored.
- Classic subscription administrators, PIM-eligible (unactivated)
  assignments and Entra directory roles are not considered.
- The live path reflects only the authenticated ARM identity, and only
  resource-group and resource scopes are supported by the permissions API.
