// Package blob implements the golly VFS (Virtual File System) interface for
// Azure Blob Storage.
//
// It registers itself with the golly VFS manager on import, supporting the
// "azblob" URL scheme. URLs follow the format:
//
//	azblob://<storage-account>/<container>/<path/to/blob>
//
// Configuration is resolved via the azcfg package, keyed by the storage
// account name (the URL host), then the "azblob" scheme name, then
// azcfg.DefaultConfigName. With nothing registered, DefaultAzureCredential
// is used against https://<account>.blob.core.windows.net/.
//
//	cfg := azcfg.NewConfig() // DefaultAzureCredential
//	azcfg.Manager.Register("mystorageaccount", cfg)
//
//	// Azurite / emulator:
//	dev := azcfg.NewConfig()
//	dev.SetEndpoint("http://127.0.0.1:10000/devstoreaccount1")
//	dev.SetSharedKey("devstoreaccount1", "<well-known key>")
//	azcfg.Manager.Register("devstoreaccount1", dev)
//
//	// Then use via the VFS manager:
//	file, err := vfs.GetManager().OpenRaw("azblob://mystorageaccount/data/path/to/file.txt")
//
// Supported auth: token credential (default, via azcfg.Config.Credential),
// connection string, shared key, and SAS (an Endpoint carrying a query
// string). Directories are virtual; Mkdir writes a zero-byte marker blob
// whose name ends in "/", and Mkdir on a container root creates the
// container.
//
// BlobFS implements vfs.VFileSystemCtx and vfs.Lister, and BlobFile
// implements vfs.RangeReader. Errors are mapped to vfs.ErrNotExist,
// vfs.ErrPermission and fs.ErrExist while keeping the SDK error in the
// chain. The blobtest sub-package provides an in-memory fake Blob service
// for offline tests.
package blob
