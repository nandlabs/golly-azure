// Package entra provides a Microsoft Entra ID (formerly Azure Active
// Directory) adapter for the golly auth primitives.
//
// It ships these building blocks that plug into oss.nandlabs.io/golly/auth:
//
//   - Verifier: a JWKS-backed JWT verifier for id and access tokens minted
//     by Entra ID. It fetches the tenant signing keys from
//     https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys (or
//     resolves jwks_uri through the tenant's OpenID discovery document
//     with WithOpenIDDiscovery), caches them per-kid with a configurable
//     TTL, refreshes on unknown kid (rate-limited), and validates the
//     RS256 signature plus exp/nbf/iat, the token version (ver 1.0 or
//     2.0), the matching issuer format (https://sts.windows.net/<tid>/ for
//     v1.0, https://login.microsoftonline.com/<tid>/v2.0 for v2.0), the
//     tid claim, the audience and a non-empty sub. A tenant GUID pins the
//     verifier to one tenant; "common"/"organizations" enable multi-tenant
//     mode, which requires a tenant allow-list (WithAllowedTenants) or an
//     explicit WithAnyTenant.
//
//   - Claim helpers (ObjectID, TenantID, Scopes, Roles, Groups, ...) and
//     Principal, an authz.Principal built from verified claims whose ID()
//     is the object id and whose Groups() are the token's group ids — the
//     shape consumed by the golly-azure authz/azrbac evaluator.
//
//   - TableSessionStore: an auth.SessionStore backed by Azure Table
//     Storage (aztables; Azurite and the Cosmos DB Table API work too).
//     Sessions are stored as JSON in an entity keyed by session id; the
//     subject and expiry are mirrored as properties so ListByUser can
//     filter server-side. Table Storage has no native TTL, so expiry is
//     enforced on read and PurgeExpired sweeps expired entities.
//
// Neither type registers itself with any manager: callers wire them
// explicitly into their golly auth verification or session middleware.
//
// Microsoft Graph access tokens (aud 00000003-0000-0000-c000-000000000000)
// carry a nonce header and are intended for Graph only; they cannot be
// validated by third parties and will fail signature verification.
package entra
