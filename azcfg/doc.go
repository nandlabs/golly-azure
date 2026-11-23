// Package azcfg provides a centralized Azure configuration management layer for golly-azure.
//
// It follows the same provider/manager pattern used by golly-aws's awscfg and
// golly-gcp's gcpsvc packages, allowing multiple named Azure configurations
// (tenant, subscription, credentials, endpoints, cloud) to be registered and
// resolved by name or by URL.
//
// Usage:
//
//	cfg := azcfg.NewConfig()
//	cfg.SetTenantID("00000000-0000-0000-0000-000000000000")
//	cfg.SetSubscriptionID("11111111-1111-1111-1111-111111111111")
//	azcfg.Manager.Register("default", cfg)
//
//	// Later, retrieve it and build a credential for any Azure SDK client:
//	cfg := azcfg.Manager.Get("default")
//	cred, err := cfg.Credential()
//
// When no explicit credential is configured, Credential returns an
// azidentity.DefaultAzureCredential, which picks up environment variables,
// workload identity, managed identity, and the Azure CLI / azd login.
package azcfg
