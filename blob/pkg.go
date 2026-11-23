package blob

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/l3"
	"oss.nandlabs.io/golly/vfs"
)

var logger = l3.Get()

func init() {
	blobFs := &BlobFS{}
	blobFs.BaseVFS = &vfs.BaseVFS{VFileSystem: blobFs}
	vfs.GetManager().Register(blobFs)
}

// fallbackConfig is used when no azcfg.Config is registered for the URL,
// for the "azblob" scheme name, or as azcfg.DefaultConfigName. It is
// created once so the DefaultAzureCredential (and its token cache) is
// shared across calls instead of being rebuilt on every operation.
var fallbackConfig = sync.OnceValue(azcfg.NewConfig)

// resolveConfig finds the azcfg.Config for the given URL.
//
// Resolution order:
//  1. azcfg.GetConfig(u, "azblob") — a config registered under the storage
//     account name (URL host), then one registered as "azblob"
//  2. a config registered as azcfg.DefaultConfigName ("default")
//  3. a package-level azcfg.NewConfig() (DefaultAzureCredential)
func resolveConfig(opts *urlOpts) *azcfg.Config {
	if cfg := azcfg.GetConfig(opts.u, BlobScheme); cfg != nil {
		return cfg
	}
	if cfg := azcfg.Manager.Get(azcfg.DefaultConfigName); cfg != nil {
		return cfg
	}
	return fallbackConfig()
}

// getContainerClient returns a container client for the account and
// container named by opts, using the azcfg config resolved for the URL.
func getContainerClient(opts *urlOpts) (*container.Client, error) {
	svc, err := newServiceClient(resolveConfig(opts), opts.Account)
	if err != nil {
		return nil, err
	}
	return svc.NewContainerClient(opts.Container), nil
}

// newServiceClient builds an Azure Blob service client for account from cfg.
//
// Authentication is chosen in this order:
//  1. cfg.ConnectionString — service.NewClientFromConnectionString
//  2. cfg.AccountKey       — shared key (account name from cfg.AccountName, else the URL host)
//  3. cfg.Endpoint carrying a query string (a SAS token) — anonymous / SAS client
//  4. otherwise            — cfg.Credential() token credential (DefaultAzureCredential by default)
//
// The service URL is cfg.Endpoint when set (e.g. Azurite's
// http://127.0.0.1:10000/devstoreaccount1), otherwise
// https://<account>.blob.<cloud storage suffix>/.
func newServiceClient(cfg *azcfg.Config, account string) (*service.Client, error) {
	if cfg == nil {
		return nil, errors.New("azblob: nil config")
	}
	clientOpts := &service.ClientOptions{ClientOptions: cfg.ClientOpts()}

	if cfg.ConnectionString != "" {
		return service.NewClientFromConnectionString(cfg.ConnectionString, clientOpts)
	}

	serviceURL := cfg.Endpoint
	if serviceURL == "" {
		if account == "" {
			return nil, errors.New("azblob: storage account name is required")
		}
		serviceURL = fmt.Sprintf("https://%s.blob.%s/", account, storageSuffix(cfg.Cloud))
	}

	if cfg.AccountKey != "" {
		name := cfg.AccountName
		if name == "" {
			name = account
		}
		cred, err := service.NewSharedKeyCredential(name, cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("azblob: invalid shared key: %w", err)
		}
		return service.NewClientWithSharedKeyCredential(serviceURL, cred, clientOpts)
	}

	if strings.Contains(serviceURL, "?") {
		// SAS URL — the token in the query string authorises requests.
		return service.NewClientWithNoCredential(serviceURL, clientOpts)
	}

	cred, err := cfg.Credential()
	if err != nil {
		return nil, err
	}
	return service.NewClient(serviceURL, cred, clientOpts)
}

// storageSuffix returns the blob endpoint DNS suffix for the selected cloud.
func storageSuffix(c cloud.Configuration) string {
	switch c.ActiveDirectoryAuthorityHost {
	case cloud.AzureChina.ActiveDirectoryAuthorityHost:
		return "core.chinacloudapi.cn"
	case cloud.AzureGovernment.ActiveDirectoryAuthorityHost:
		return "core.usgovcloudapi.net"
	default:
		return "core.windows.net"
	}
}
