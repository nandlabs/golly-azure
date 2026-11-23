// Package azredis provides an oss.nandlabs.io/golly/cache.Cache backend
// for Azure Cache for Redis and Azure Managed Redis. It wraps go-redis/v9's
// universal client so the same code works against:
//
//   - Azure Cache for Redis Basic / Standard / Premium (single endpoint,
//     <name>.redis.cache.windows.net:6380 over TLS).
//   - Azure Cache for Redis Premium with clustering, and Azure Managed
//     Redis with the OSS cluster policy (set Config.ClusterMode so go-redis
//     discovers shards from the single endpoint).
//   - Azure Managed Redis with the Enterprise cluster policy
//     (<name>.<region>.redis.azure.net:10000; a single proxied endpoint —
//     leave ClusterMode false).
//
// Two authentication modes are supported:
//
//   - Access keys: set Config.Password to the primary or secondary access
//     key (Username empty, or "default").
//   - Microsoft Entra ID: use NewWithEntraID (or an EntraIDProvider wired
//     into Config.StreamingCredentialsProvider). Tokens for the scope
//     https://redis.azure.com/.default are obtained from an
//     azcore.TokenCredential (resolved through azcfg by default), the
//     Redis username is the principal's object ID (the token's "oid"
//     claim), and tokens are refreshed in the background before they
//     expire. Refreshed tokens are pushed to go-redis, which re-AUTHs
//     pooled connections — Azure closes connections whose token expires.
//
// TLS is on by default (MinVersion TLS 1.2); addresses without a port get
// DefaultTLSPort (6380). Azure Managed Redis listens on ManagedRedisPort
// (10000) — include it in the address or use ManagedRedisEndpoint.
//
// Client also satisfies the optional cache.Sweeper (no-op; Redis expires
// keys itself) and cache.Loader (in-process single-flight) capabilities.
package azredis
