package azcfg

import (
	"errors"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"oss.nandlabs.io/golly/managers"
)

// DefaultConfigName is the name used to look up the fallback configuration
// when a URL does not resolve to a registered configuration.
const DefaultConfigName = "default"

// ErrIncompleteClientSecret is returned when client-secret credentials are only partially configured.
var ErrIncompleteClientSecret = errors.New("azcfg: tenant id, client id and client secret are all required for client secret credentials")

// Config holds Azure configuration options used to build credentials and
// client options for the Azure SDK for Go.
type Config struct {
	// TenantID is the Microsoft Entra ID tenant (directory) ID.
	TenantID string
	// SubscriptionID is the Azure subscription ID used by ARM-based packages (e.g. authz/azrbac).
	SubscriptionID string
	// ResourceGroup is an optional default resource group.
	ResourceGroup string
	// Location is an optional default Azure region (e.g. "eastus").
	Location string
	// ClientID is the application (client) ID for service principal or user-assigned managed identity auth.
	ClientID string
	// ClientSecret is the client secret for service principal auth.
	ClientSecret string
	// UseManagedIdentity forces managed identity auth (system-assigned, or user-assigned when ClientID is set).
	UseManagedIdentity bool
	// ConnectionString is an optional service connection string (Storage, Service Bus, ...).
	// Packages that support connection strings prefer it over token credentials when set.
	ConnectionString string
	// AccountName is an optional default account/namespace name (storage account, Service Bus namespace, ...).
	AccountName string
	// AccountKey is an optional shared key (Storage account key, ...).
	AccountKey string
	// Endpoint is an optional custom service endpoint (useful for Azurite and other emulators).
	Endpoint string
	// Cloud selects the Azure cloud (public, government, China). Defaults to cloud.AzurePublic.
	Cloud cloud.Configuration
	// ClientOptions holds additional azcore client options (retry, transport, telemetry, ...).
	ClientOptions policy.ClientOptions
	// TokenCredential, when set, is returned as-is by Credential and overrides all other auth settings.
	TokenCredential azcore.TokenCredential

	mu   sync.Mutex
	cred azcore.TokenCredential
}

// NewConfig creates an empty Config that uses DefaultAzureCredential.
func NewConfig() *Config {
	return &Config{}
}

// SetTenantID sets the Entra ID tenant ID.
func (c *Config) SetTenantID(tenantID string) { c.TenantID = tenantID; c.reset() }

// SetSubscriptionID sets the Azure subscription ID.
func (c *Config) SetSubscriptionID(subscriptionID string) { c.SubscriptionID = subscriptionID }

// SetResourceGroup sets the default resource group.
func (c *Config) SetResourceGroup(rg string) { c.ResourceGroup = rg }

// SetLocation sets the default Azure region.
func (c *Config) SetLocation(location string) { c.Location = location }

// SetClientSecret configures service principal (client secret) credentials.
func (c *Config) SetClientSecret(tenantID, clientID, clientSecret string) {
	c.TenantID, c.ClientID, c.ClientSecret = tenantID, clientID, clientSecret
	c.UseManagedIdentity = false
	c.reset()
}

// SetManagedIdentity configures managed identity auth. Pass an empty clientID for the system-assigned identity.
func (c *Config) SetManagedIdentity(clientID string) {
	c.UseManagedIdentity = true
	c.ClientID = clientID
	c.ClientSecret = ""
	c.reset()
}

// SetCredential sets an explicit token credential that overrides all other auth settings.
func (c *Config) SetCredential(cred azcore.TokenCredential) { c.TokenCredential = cred; c.reset() }

// SetConnectionString sets a service connection string.
func (c *Config) SetConnectionString(cs string) { c.ConnectionString = cs }

// SetSharedKey sets a storage account name and key.
func (c *Config) SetSharedKey(accountName, accountKey string) {
	c.AccountName, c.AccountKey = accountName, accountKey
}

// SetAccountName sets the default account / namespace name.
func (c *Config) SetAccountName(name string) { c.AccountName = name }

// SetEndpoint sets a custom service endpoint.
func (c *Config) SetEndpoint(endpoint string) { c.Endpoint = endpoint }

// SetCloud selects the Azure cloud (cloud.AzurePublic, cloud.AzureGovernment, cloud.AzureChina).
func (c *Config) SetCloud(cfg cloud.Configuration) { c.Cloud = cfg; c.reset() }

// ClientOpts returns azcore client options with the configured cloud applied.
func (c *Config) ClientOpts() policy.ClientOptions {
	opts := c.ClientOptions
	if c.Cloud.ActiveDirectoryAuthorityHost != "" {
		opts.Cloud = c.Cloud
	}
	return opts
}

// Credential returns an azcore.TokenCredential built from the configuration.
// The credential is created once and cached for the lifetime of the Config.
//
// Resolution order:
//  1. TokenCredential, if set
//  2. Client secret credentials, if ClientSecret is set
//  3. Managed identity, if UseManagedIdentity is set
//  4. azidentity.DefaultAzureCredential
func (c *Config) Credential() (azcore.TokenCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.TokenCredential != nil {
		return c.TokenCredential, nil
	}
	if c.cred != nil {
		return c.cred, nil
	}
	cred, err := c.buildCredential()
	if err != nil {
		return nil, err
	}
	c.cred = cred
	return cred, nil
}

func (c *Config) buildCredential() (azcore.TokenCredential, error) {
	opts := c.ClientOpts()
	switch {
	case c.ClientSecret != "":
		if c.TenantID == "" || c.ClientID == "" {
			return nil, ErrIncompleteClientSecret
		}
		return azidentity.NewClientSecretCredential(c.TenantID, c.ClientID, c.ClientSecret,
			&azidentity.ClientSecretCredentialOptions{ClientOptions: opts})
	case c.UseManagedIdentity:
		miOpts := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: opts}
		if c.ClientID != "" {
			miOpts.ID = azidentity.ClientID(c.ClientID)
		}
		return azidentity.NewManagedIdentityCredential(miOpts)
	default:
		return azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
			ClientOptions: opts,
			TenantID:      c.TenantID,
		})
	}
}

func (c *Config) reset() {
	c.mu.Lock()
	c.cred = nil
	c.mu.Unlock()
}

// Manager is an item manager for Config instances, allowing named registration and retrieval.
var Manager = managers.NewItemManager[*Config]()
