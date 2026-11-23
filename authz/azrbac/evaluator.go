package azrbac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"oss.nandlabs.io/golly/authz"
)

// DefaultRoleDefinitionTTL is how long a resolved role definition is
// reused across refreshes before it is fetched again. Built-in roles
// change rarely; custom roles are picked up within this window.
const DefaultRoleDefinitionTTL = time.Hour

// PrincipalMemberFunc extracts an Entra object id (the principalId of
// role assignments) from an authz.Principal.
type PrincipalMemberFunc func(p authz.Principal) string

// PrincipalGroupsFunc extracts the Entra group object ids a principal
// is a member of. Role assignments made to any of these groups apply
// to the principal.
type PrincipalGroupsFunc func(p authz.Principal) []string

// RBACResource is passed as the `resource any` argument to
// Evaluator.Can when the caller wants a live check instead of the
// cached evaluation. It carries the context and the ARM scope.
//
// The live check calls Microsoft.Authorization/permissions, which
// reports the effective permissions of the identity the ARM client is
// authenticated as — NOT of the Principal handed to Can. Combine it
// with WithExpectedMember(<that identity's object id>) so other
// principals are denied instead of silently inheriting the app's
// permissions.
type RBACResource struct {
	// Ctx is the request context used for the live call. If nil,
	// context.Background() is used.
	Ctx context.Context
	// Scope is the ARM scope to check, e.g.
	// "/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Storage/storageAccounts/<acct>".
	// If empty, the evaluator's configured scope is used. The
	// permissions API supports resource-group and resource scopes only.
	Scope string
}

// Option customises an Evaluator at construction time.
type Option func(*Evaluator)

// WithPrincipalMember overrides how the principal's object id is
// derived. The default tries interface{ ObjectID() string }, then
// interface{ ID() string }, then interface{ Member() string }.
func WithPrincipalMember(fn PrincipalMemberFunc) Option {
	return func(e *Evaluator) {
		if fn != nil {
			e.extractor = fn
		}
	}
}

// WithPrincipalGroups overrides how the principal's group object ids
// are derived. The default uses interface{ Groups() []string } (as
// implemented by golly-azure auth/entra.Principal, which surfaces the
// token's groups claim).
func WithPrincipalGroups(fn PrincipalGroupsFunc) Option {
	return func(e *Evaluator) {
		if fn != nil {
			e.groups = fn
		}
	}
}

// WithExpectedMember restricts Can to grant only when the extracted
// object id equals member. Recommended whenever RBACResource live
// checks are used (see RBACResource).
func WithExpectedMember(member string) Option {
	return func(e *Evaluator) { e.expected = strings.ToLower(member) }
}

// WithDataActions registers actions that Can must treat as data-plane
// actions (DataActions/NotDataActions) even without the "data:" prefix.
func WithDataActions(actions ...string) Option {
	return func(e *Evaluator) {
		for _, a := range actions {
			if a != "" {
				e.dataActions[strings.ToLower(a)] = struct{}{}
			}
		}
	}
}

// WithoutDenyAssignments skips listing deny assignments during Refresh.
// Use it only when the identity lacks
// Microsoft.Authorization/denyAssignments/read and you accept that
// deny assignments (Blueprints, managed apps, deployment stacks) are
// not honoured.
func WithoutDenyAssignments() Option { return func(e *Evaluator) { e.denies = false } }

// WithRoleDefinitionTTL sets how long resolved role definitions are
// reused across refreshes.
func WithRoleDefinitionTTL(d time.Duration) Option {
	return func(e *Evaluator) {
		if d > 0 {
			e.defTTL = d
		}
	}
}

// WithMaxStaleness makes Can fail closed (deny) when the last
// successful Refresh is older than d — e.g. because ARM has been
// unreachable. Zero (the default) keeps serving the last snapshot
// indefinitely, matching the other golly cloud evaluators.
func WithMaxStaleness(d time.Duration) Option {
	return func(e *Evaluator) {
		if d >= 0 {
			e.maxStale = d
		}
	}
}

// WithClock overrides the time source. Primarily used by tests.
func WithClock(now func() time.Time) Option {
	return func(e *Evaluator) {
		if now != nil {
			e.now = now
		}
	}
}

// Evaluator implements authz.Policy on top of Azure RBAC.
//
// Azure has no "simulate principal" API, so the evaluator reproduces
// the RBAC decision locally: Refresh lists every role assignment at or
// above the configured scope (for all principals), resolves the role
// definitions' Actions/NotActions/DataActions/NotDataActions, and lists
// deny assignments at or above the scope. Can then grants iff an
// unconditional assignment to the principal's object id (or one of its
// groups) grants the action at the target scope and no deny assignment
// blocks it. A background goroutine re-runs Refresh every `refresh`
// interval.
//
// Can also accepts an RBACResource / *RBACResource as its `resource
// any` argument to force a live Microsoft.Authorization/permissions
// check — see RBACResource.
type Evaluator struct {
	caller    RBACCaller
	scope     string // normalized base scope
	refresh   time.Duration
	extractor PrincipalMemberFunc
	groups    PrincipalGroupsFunc
	expected  string

	dataActions map[string]struct{}
	denies      bool
	defTTL      time.Duration
	maxStale    time.Duration
	now         func() time.Time

	mu   sync.RWMutex
	snap *snapshot

	defMu sync.Mutex
	defs  map[string]cachedDef // lower(roleDefinitionID) -> def

	stopCh   chan struct{}
	doneCh   chan struct{}
	closedMu sync.Mutex
	closed   bool
}

type cachedDef struct {
	perms   []Permission
	expires time.Time
}

type grant struct {
	principal string // lower-case object id
	scope     string // normalized
	perms     []Permission
}

type deny struct {
	scope      string
	perms      []Permission
	principals map[string]struct{}
	excluded   map[string]struct{}
	everyone   bool
	noChildren bool
}

type snapshot struct {
	grants   []grant
	denies   []deny
	loadedAt time.Time
}

// NewCachedEvaluator wires the evaluator to an *ARMCaller. scope is the
// base ARM scope whose effective assignments are cached (a subscription,
// resource group or resource id). Pass 0 (or negative) for refresh to
// disable the background goroutine — the caller then invokes Refresh.
func NewCachedEvaluator(caller *ARMCaller, scope string, refresh time.Duration, opts ...Option) *Evaluator {
	return NewCachedEvaluatorFromCaller(caller, scope, refresh, opts...)
}

// NewCachedEvaluatorFromCaller is the general constructor: any
// RBACCaller can be plugged in. Tests use it to inject a fake.
//
// No network call is made here; Can denies everything until the first
// successful Refresh. Call Refresh once at start-up to fail fast.
func NewCachedEvaluatorFromCaller(caller RBACCaller, scope string, refresh time.Duration, opts ...Option) *Evaluator {
	e := &Evaluator{
		caller:      caller,
		scope:       normalizeScope(scope),
		refresh:     refresh,
		extractor:   defaultPrincipalMember,
		groups:      defaultPrincipalGroups,
		dataActions: map[string]struct{}{},
		denies:      true,
		defTTL:      DefaultRoleDefinitionTTL,
		now:         time.Now,
		defs:        map[string]cachedDef{},
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	if refresh > 0 {
		go e.loop()
	} else {
		close(e.doneCh)
	}
	return e
}

func defaultPrincipalMember(p authz.Principal) string {
	if p == nil {
		return ""
	}
	if v, ok := p.(interface{ ObjectID() string }); ok {
		return v.ObjectID()
	}
	if v, ok := p.(interface{ ID() string }); ok {
		return v.ID()
	}
	if v, ok := p.(interface{ Member() string }); ok {
		return v.Member()
	}
	return ""
}

func defaultPrincipalGroups(p authz.Principal) []string {
	if p == nil {
		return nil
	}
	if v, ok := p.(interface{ Groups() []string }); ok {
		return v.Groups()
	}
	return nil
}

// Scope returns the normalized base scope.
func (e *Evaluator) Scope() string { return e.scope }

func (e *Evaluator) loop() {
	defer close(e.doneCh)
	t := time.NewTicker(e.refresh)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), e.refresh)
			_ = e.Refresh(ctx)
			cancel()
		}
	}
}

// Refresh reloads role assignments, role definitions and (unless
// disabled) deny assignments at or above the base scope and atomically
// replaces the cached snapshot. On any error the previous snapshot is
// kept and the error is returned.
func (e *Evaluator) Refresh(ctx context.Context) error {
	if e == nil || e.caller == nil {
		return errors.New("azrbac: evaluator has no caller")
	}
	if e.scope == "" {
		return errors.New("azrbac: evaluator has no scope")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	assignments, err := e.caller.ListRoleAssignments(ctx, e.scope)
	if err != nil {
		return fmt.Errorf("azrbac: list role assignments at %q: %w", e.scope, err)
	}
	grants := make([]grant, 0, len(assignments))
	for _, a := range assignments {
		if a.PrincipalID == "" || a.RoleDefinitionID == "" {
			continue
		}
		if strings.TrimSpace(a.Condition) != "" {
			// ABAC conditions can't be evaluated locally — fail closed.
			continue
		}
		perms, err := e.roleDefinition(ctx, a.RoleDefinitionID)
		if err != nil {
			return fmt.Errorf("azrbac: resolve role definition %q: %w", a.RoleDefinitionID, err)
		}
		grants = append(grants, grant{
			principal: strings.ToLower(a.PrincipalID),
			scope:     normalizeScope(a.Scope),
			perms:     perms,
		})
	}
	var denies []deny
	if e.denies {
		das, err := e.caller.ListDenyAssignments(ctx, e.scope)
		if err != nil {
			return fmt.Errorf("azrbac: list deny assignments at %q: %w", e.scope, err)
		}
		denies = make([]deny, 0, len(das))
		for _, d := range das {
			nd := deny{
				scope:      normalizeScope(d.Scope),
				perms:      d.Permissions,
				principals: map[string]struct{}{},
				excluded:   map[string]struct{}{},
				noChildren: d.DoNotApplyToChildScopes,
			}
			for _, id := range d.Principals {
				id = strings.ToLower(id)
				if id == EveryonePrincipalID {
					nd.everyone = true
				}
				nd.principals[id] = struct{}{}
			}
			for _, id := range d.ExcludePrincipals {
				nd.excluded[strings.ToLower(id)] = struct{}{}
			}
			denies = append(denies, nd)
		}
	}
	e.mu.Lock()
	e.snap = &snapshot{grants: grants, denies: denies, loadedAt: e.now()}
	e.mu.Unlock()
	return nil
}

func (e *Evaluator) roleDefinition(ctx context.Context, id string) ([]Permission, error) {
	key := strings.ToLower(id)
	now := e.now()
	e.defMu.Lock()
	c, ok := e.defs[key]
	e.defMu.Unlock()
	if ok && now.Before(c.expires) {
		return c.perms, nil
	}
	def, err := e.caller.GetRoleDefinition(ctx, id)
	if err != nil {
		return nil, err
	}
	e.defMu.Lock()
	e.defs[key] = cachedDef{perms: def.Permissions, expires: now.Add(e.defTTL)}
	e.defMu.Unlock()
	return def.Permissions, nil
}

// Can satisfies authz.Policy.
//
// resource selects the target scope:
//   - RBACResource / *RBACResource: live Microsoft.Authorization/permissions
//     check for the client identity (see RBACResource);
//   - a string starting with "/": evaluate the cache at that ARM scope;
//   - anything else (including nil): evaluate at the base scope.
//
// action is an Azure RBAC operation such as
// "Microsoft.Compute/virtualMachines/start/action". Prefix it with
// DataActionPrefix (see DataAction) or register it with WithDataActions
// to evaluate it as a data action.
func (e *Evaluator) Can(p authz.Principal, action string, resource any) bool {
	if e == nil || action == "" {
		return false
	}
	member := strings.ToLower(e.extractor(p))
	if member == "" {
		return false
	}
	if e.expected != "" && member != e.expected {
		return false
	}
	act, data := e.classify(action)
	if act == "" {
		return false
	}
	if ctx, scope, ok := extractRBACResource(resource); ok {
		return e.liveCheck(ctx, scope, act, data)
	}
	target := e.scope
	if s, ok := resource.(string); ok && strings.HasPrefix(s, "/") {
		target = normalizeScope(s)
	}
	ids := map[string]struct{}{member: {}}
	for _, g := range e.groups(p) {
		if g != "" {
			ids[strings.ToLower(g)] = struct{}{}
		}
	}
	return e.evaluate(ids, target, act, data)
}

// Evaluate is the context-free core of Can exposed for callers that
// already hold the object id and group ids (e.g. from verified Entra
// claims). scope "" means the base scope.
func (e *Evaluator) Evaluate(objectID string, groupIDs []string, scope, action string) bool {
	if e == nil || objectID == "" || action == "" {
		return false
	}
	act, data := e.classify(action)
	if act == "" {
		return false
	}
	ids := map[string]struct{}{strings.ToLower(objectID): {}}
	for _, g := range groupIDs {
		if g != "" {
			ids[strings.ToLower(g)] = struct{}{}
		}
	}
	target := e.scope
	if scope != "" {
		target = normalizeScope(scope)
	}
	return e.evaluate(ids, target, act, data)
}

func (e *Evaluator) classify(action string) (string, bool) {
	if len(action) >= len(DataActionPrefix) && strings.EqualFold(action[:len(DataActionPrefix)], DataActionPrefix) {
		return action[len(DataActionPrefix):], true
	}
	if _, ok := e.dataActions[strings.ToLower(action)]; ok {
		return action, true
	}
	return action, false
}

func (e *Evaluator) evaluate(ids map[string]struct{}, target, action string, data bool) bool {
	e.mu.RLock()
	snap := e.snap
	e.mu.RUnlock()
	if snap == nil {
		return false
	}
	if e.maxStale > 0 && e.now().Sub(snap.loadedAt) > e.maxStale {
		return false
	}
	// Every cached assignment is at or above the base scope (atScope()),
	// so all of them apply to the base scope and anything below it,
	// including assignments at management-group scopes whose ids are not
	// textual prefixes of subscription scopes. For targets outside the
	// base scope fall back to textual containment.
	inBase := scopeContains(e.scope, target)
	allowed := false
	for _, g := range snap.grants {
		if _, ok := ids[g.principal]; !ok {
			continue
		}
		if !inBase && !scopeContains(g.scope, target) {
			continue
		}
		if allows(g.perms, action, data) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	for _, d := range snap.denies {
		if !d.appliesTo(ids) {
			continue
		}
		if d.noChildren {
			if target != d.scope {
				continue
			}
		} else if !inBase && !scopeContains(d.scope, target) {
			continue
		}
		if allows(d.perms, action, data) {
			return false
		}
	}
	return true
}

func (d deny) appliesTo(ids map[string]struct{}) bool {
	for id := range ids {
		if _, ok := d.excluded[id]; ok {
			return false
		}
	}
	if d.everyone {
		return true
	}
	for id := range ids {
		if _, ok := d.principals[id]; ok {
			return true
		}
	}
	return false
}

func (e *Evaluator) liveCheck(ctx context.Context, scope, action string, data bool) bool {
	if e.caller == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if scope == "" {
		scope = e.scope
	}
	perms, err := e.caller.ListPermissions(ctx, scope)
	if err != nil {
		return false
	}
	return allows(perms, action, data)
}

// extractRBACResource unwraps RBACResource / *RBACResource from the any.
func extractRBACResource(resource any) (context.Context, string, bool) {
	switch r := resource.(type) {
	case RBACResource:
		return r.Ctx, r.Scope, true
	case *RBACResource:
		if r == nil {
			return nil, "", false
		}
		return r.Ctx, r.Scope, true
	}
	return nil, "", false
}

// Close stops the background refresh goroutine. Safe to call multiple
// times.
func (e *Evaluator) Close() error {
	if e == nil {
		return nil
	}
	e.closedMu.Lock()
	if e.closed {
		e.closedMu.Unlock()
		return nil
	}
	e.closed = true
	close(e.stopCh)
	e.closedMu.Unlock()
	<-e.doneCh
	return nil
}
