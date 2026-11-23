# blob

Azure Blob Storage implementation of the [golly VFS](https://pkg.go.dev/oss.nandlabs.io/golly/vfs) (Virtual File System) interface.

---

- [Installation](#installation)
- [Features](#features)
- [Architecture](#architecture)
- [Auto-Registration](#auto-registration)
- [URL Format](#url-format)
- [Configuration](#configuration)
  - [How It Works](#how-it-works)
  - [Authentication](#authentication)
  - [Setup Examples](#setup-examples)
- [Usage](#usage)
- [Error Handling](#error-handling)
- [API Reference](#api-reference)
- [Testing](#testing)
- [Prerequisites](#prerequisites)
- [Contributing](#contributing)

---

## Installation

```bash
go get oss.nandlabs.io/golly-azure/blob
```

## Features

### File Operations

- **Read** — stream blob content (lazy ranged download from the current offset)
- **Write** — buffered writes uploaded as a block blob on `Close()`
- **Seek** — `SeekStart`, `SeekCurrent` and `SeekEnd` for reads (the next `Read` issues a ranged GET)
- **ReadRange** — native ranged GET via `vfs.ReadRange` (`vfs.RangeReader`)
- **Delete** — delete a single blob (and its snapshots)
- **DeleteAll** — delete every blob under a prefix, plus the directory marker
- **ListAll** — list all blobs (recursively) under a prefix
- **Info** — size, last modified, content type, ETag and directory detection
- **Parent** — navigate to the parent prefix
- **AddProperty / GetProperty** — read and write blob user metadata (case-insensitive, ETag-guarded)
- **ContentType** — MIME type of the blob (guessed from the extension on upload)

### File System Operations

- **Create** — atomically create a new empty blob (`If-None-Match: *`; fails with `fs.ErrExist` if present)
- **Open** — open a blob for reading/writing (no network call)
- **Mkdir / MkdirAll** — create directory markers (zero-byte blobs ending in `/`); on a container root, create the container; a missing container is created on demand
- **Copy** — server-side Copy Blob (awaits pending copies), with stream fallback for cross-account copies
- **Move** — copy + delete (moving a URL onto itself is a no-op)
- **Delete** — delete a blob, or recursively delete a prefix
- **List** — list direct children of a prefix (blobs and virtual directories)
- **ListIter** — lazily paginated listing (`vfs.Lister`)
- **Walk** — traverse every blob under a prefix, honouring `vfs.ErrSkipDir` / `vfs.ErrSkipAll`
- **Find** — filter blobs using a custom `FileFilter` function
- **DeleteMatching** — delete blobs matching a filter
- **Context variants** — `OpenCtx`, `CreateCtx`, `MkdirAllCtx`, `DeleteCtx`, `ListCtx`, `WalkCtx`, `CopyCtx`, `MoveCtx` (`vfs.VFileSystemCtx`)

All operations also have `*Raw` variants that accept URL strings instead of `*url.URL`.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Application                                                     │
│                                                                  │
│  import _ "oss.nandlabs.io/golly-azure/blob"                     │
│                                                                  │
│  mgr := vfs.GetManager()                                         │
│  mgr.OpenRaw("azblob://account/container/key")                   │
│  mgr.CopyRaw(src, dst)                                           │
└─────────────────────────┬────────────────────────────────────────┘
                          │
                          ▼
┌──────────────────────────────────────────────────────────────────┐
│  golly/vfs.Manager                                               │
│                                                                  │
│  Routes to filesystem by URL scheme ("azblob")                   │
└─────────────────────────┬────────────────────────────────────────┘
                          │
                          ▼
┌──────────────────────────────────────────────────────────────────┐
│  blob.BlobFS (VFileSystem, VFileSystemCtx, Lister)               │
│                                                                  │
│  1. parseURL(u)          → account + container + key             │
│  2. resolveConfig(opts)  → azcfg.GetConfig(u, "azblob")          │
│  3. newServiceClient     → conn string / shared key / SAS / token│
│  4. Blob REST call       → Put/Get/List/Copy/Delete Blob         │
│  5. Returns BlobFile     → VFile + RangeReader                   │
└─────────────────────────┬────────────────────────────────────────┘
                          │
                          ▼
┌──────────────────────────────────────────────────────────────────┐
│  azcfg.Manager                                                   │
│                                                                  │
│  url.Host (account) → "azblob" → "default" → DefaultAzureCred    │
└──────────────────────────────────────────────────────────────────┘
```

## Auto-Registration

On package import, `init()` in `pkg.go` creates a `BlobFS` and registers it with `vfs.GetManager()`:

```go
func init() {
    blobFs := &BlobFS{}
    blobFs.BaseVFS = &vfs.BaseVFS{VFileSystem: blobFs}
    vfs.GetManager().Register(blobFs)
}
```

A **blank import** is all you need:

```go
import _ "oss.nandlabs.io/golly-azure/blob"
```

## URL Format

```
azblob://<storage-account>/<container>/path/to/blob.txt
azblob://<storage-account>/<container>/path/to/folder/
```

| Component    | Maps To                              |
| ------------ | ------------------------------------ |
| Scheme       | `azblob`                             |
| Host         | Storage account name                 |
| First segment | Container name (required)          |
| Rest of path | Blob name (prefix + filename)        |

**Examples:**

| URL                                          | Account     | Container | Blob                   | Type      |
| -------------------------------------------- | ----------- | --------- | ---------------------- | --------- |
| `azblob://myacct/data/report.csv`            | `myacct`    | `data`    | `report.csv`           | File      |
| `azblob://myacct/data/logs/`                 | `myacct`    | `data`    | `logs/`                | Directory |
| `azblob://myacct/archive/2026/jan.zip`       | `myacct`    | `archive` | `2026/jan.zip`         | File      |
| `azblob://myacct/backup`                     | `myacct`    | `backup`  | _(empty — container root)_ | Directory |

## Configuration

blob uses the [`azcfg`](../azcfg/) package. Register one or more `*azcfg.Config` instances with `azcfg.Manager`; blob picks the right one for each URL.

### How It Works

For `azblob://myacct/data/file.txt` the config is resolved in this order:

| Step | Lookup                                   | Purpose                          |
| ---- | ---------------------------------------- | -------------------------------- |
| 1    | `azcfg.Manager.Get("myacct")`            | Storage-account-specific config  |
| 2    | `azcfg.Manager.Get("azblob")`            | Default for all blob operations  |
| 3    | `azcfg.Manager.Get("default")`           | Default for all golly-azure pkgs |
| 4    | package-level `azcfg.NewConfig()`        | `DefaultAzureCredential`         |

(Steps 1–2 are `azcfg.GetConfig(u, "azblob")`.)

### Authentication

`newServiceClient` picks the auth mode from the resolved config:

| Priority | Config                                   | Client                                                    |
| -------- | ---------------------------------------- | --------------------------------------------------------- |
| 1        | `ConnectionString`                       | `service.NewClientFromConnectionString`                   |
| 2        | `AccountKey` (+ `AccountName` or URL host) | Shared key credential                                   |
| 3        | `Endpoint` containing `?` (SAS token)    | `service.NewClientWithNoCredential` (SAS in query string) |
| 4        | anything else                            | `cfg.Credential()` token credential (Entra ID)            |

The service URL is `cfg.Endpoint` when set (e.g. Azurite's `http://127.0.0.1:10000/devstoreaccount1`), otherwise `https://<account>.blob.core.windows.net/` (or the China / US Government suffix when `cfg.Cloud` is set). `cfg.ClientOpts()` (retry, transport, cloud) is applied to every client.

### Setup Examples

#### Token credential (default)

```go
import (
    "oss.nandlabs.io/golly-azure/azcfg"
    _ "oss.nandlabs.io/golly-azure/blob"
    "oss.nandlabs.io/golly/vfs"
)

cfg := azcfg.NewConfig() // DefaultAzureCredential: env, workload/managed identity, az CLI
azcfg.Manager.Register("mystorageacct", cfg)

f, _ := vfs.GetManager().OpenRaw("azblob://mystorageacct/data/file.txt")
```

#### Service principal / managed identity

```go
cfg := azcfg.NewConfig()
cfg.SetClientSecret(tenantID, clientID, clientSecret)
// or: cfg.SetManagedIdentity("") // system-assigned
azcfg.Manager.Register("azblob", cfg) // for every account
```

#### Connection string

```go
cfg := azcfg.NewConfig()
cfg.SetConnectionString(os.Getenv("AZURE_STORAGE_CONNECTION_STRING"))
azcfg.Manager.Register("mystorageacct", cfg)
```

#### Shared key

```go
cfg := azcfg.NewConfig()
cfg.SetSharedKey("mystorageacct", os.Getenv("AZURE_STORAGE_KEY"))
azcfg.Manager.Register("mystorageacct", cfg)
```

#### SAS URL

```go
cfg := azcfg.NewConfig()
cfg.SetEndpoint("https://mystorageacct.blob.core.windows.net/?sv=...&sig=...")
azcfg.Manager.Register("mystorageacct", cfg)
```

#### Azurite (local emulator)

```bash
docker run -p 10000:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
```

```go
cfg := azcfg.NewConfig()
cfg.SetEndpoint("http://127.0.0.1:10000/devstoreaccount1")
cfg.SetSharedKey("devstoreaccount1",
    "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
azcfg.Manager.Register("devstoreaccount1", cfg)

vfs.GetManager().MkdirAllRaw("azblob://devstoreaccount1/demo") // creates the container
```

> Token credentials require HTTPS; use a shared key or connection string against Azurite.

## Usage

### Reading a File

```go
f, err := vfs.GetManager().OpenRaw("azblob://myacct/data/hello.txt")
if err != nil { return err }
defer f.Close()
content, err := vfs.ReadAllString(f)
```

### Reading a Byte Range

```go
f, _ := vfs.GetManager().OpenRaw("azblob://myacct/data/big.parquet")
footer, err := vfs.ReadRange(ctx, f, size-8, 8) // single ranged GET
```

### Writing a File

```go
f, err := vfs.GetManager().CreateRaw("azblob://myacct/data/hello.txt")
if err != nil { return err } // fs.ErrExist if it already exists
f.WriteString("Hello, Azure!")
if err := f.Close(); err != nil { // the upload happens here
    return err
}
```

Use `OpenRaw` + `Write` + `Close` to overwrite an existing blob.

### Listing Files

```go
files, _ := vfs.GetManager().ListRaw("azblob://myacct/data/")
for _, f := range files {
    info, _ := f.Info()
    fmt.Println(info.Name(), info.IsDir())
}

// Or lazily, page by page:
it, _ := vfs.ListIter(ctx, vfs.GetManager(), u)
defer it.Close()
for f, err := it.Next(ctx); err == nil; f, err = it.Next(ctx) { /* ... */ }
```

### Walking a Directory Tree

```go
vfs.GetManager().WalkRaw("azblob://myacct/data/", func(f vfs.VFile) error {
    fmt.Println(f.Url())
    return nil
})
```

### Copying and Moving

```go
mgr.CopyRaw("azblob://myacct/data/a.txt", "azblob://myacct/backup/a.txt")
mgr.CopyRaw("azblob://myacct/data/", "azblob://otheracct/mirror/data/") // directory, cross-account
mgr.MoveRaw("azblob://myacct/data/old.txt", "azblob://myacct/archive/old.txt")
```

### Creating Directories

```go
mgr.MkdirAllRaw("azblob://myacct/data/reports/") // marker blob "reports/"
mgr.MkdirAllRaw("azblob://myacct/newcontainer")  // creates the container
```

### Deleting Files

```go
mgr.DeleteRaw("azblob://myacct/data/file.txt") // single blob
mgr.DeleteRaw("azblob://myacct/data/tmp/")     // every blob under tmp/
```

### Working with Metadata

```go
f, _ := mgr.OpenRaw("azblob://myacct/data/file.txt")
f.AddProperty("department", "engineering")
dept, _ := f.GetProperty("Department") // case-insensitive
```

### Context-aware Calls

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
err := vfs.CopyCtx(ctx, mgr, src, dst) // dispatches to BlobFS.CopyCtx
```

## Error Handling

Azure errors are wrapped with golly sentinels; the `*azcore.ResponseError` stays in the chain:

| Azure error code / status                                          | Sentinel           |
| ------------------------------------------------------------------ | ------------------ |
| `BlobNotFound`, `ContainerNotFound`, `ResourceNotFound`, HTTP 404  | `vfs.ErrNotExist`  |
| `Authorization*`, `AuthenticationFailed`, `InvalidAuthenticationInfo`, `NoAuthenticationInformation`, `InsufficientAccountPermissions`, `AccountIsDisabled`, HTTP 401/403 | `vfs.ErrPermission` |
| `BlobAlreadyExists`, `ContainerAlreadyExists` (and `Create` on an existing blob) | `fs.ErrExist` |

```go
if _, err := f.Info(); errors.Is(err, vfs.ErrNotExist) { /* ... */ }
var re *azcore.ResponseError
if errors.As(err, &re) { log.Println(re.ErrorCode) }
```

### URL Validation Errors

| Error                                                              | When                         |
| ------------------------------------------------------------------ | ---------------------------- |
| `url cannot be nil`                                                | nil `*url.URL`               |
| `invalid URL scheme, expected 'azblob'`                            | scheme is not `azblob`       |
| `invalid azblob URL, storage account name (host) is required`      | no host                      |
| `invalid azblob URL, container name (first path segment) is required` | no container segment      |

### Write Behavior

Writes are buffered in memory and uploaded when `Close()` is called (`UploadBuffer`: one Put Blob up to 256 MiB, staged blocks above). If `Close()` returns an error the data was **not** persisted. Uploading replaces the blob's metadata, as Put Blob does.

### Copy Behavior

`Copy` issues a server-side **Copy Blob** (`StartCopyFromURL`) and polls until a pending copy completes. If the service rejects it (typically a cross-account copy without a SAS on the source), it falls back to a **stream copy** (download + `UploadStream`), preserving content type and metadata.

### Directory Semantics

Blob Storage (flat namespace) has no real directories. This package simulates them with:

- **Marker blobs**: `Mkdir` writes a zero-byte blob named `dir/`
- **Virtual prefixes**: hierarchical listing (`/` delimiter) returns `BlobPrefix` entries as directories
- **Prefix detection**: if Get Blob Properties returns 404 but blobs exist under `key/`, the key is a directory
- **Container root**: `azblob://acct/container` is the root directory; `Mkdir` on it creates the container, `Delete` on it removes every blob but keeps the container

## API Reference

### BlobFS (VFileSystem, VFileSystemCtx, Lister)

| Method                      | Description                                      |
| --------------------------- | ------------------------------------------------ |
| `Schemes()`                 | Returns `["azblob"]`                             |
| `Create(u)`                 | Atomically creates a new empty blob              |
| `Open(u)`                   | Opens a blob (lazy — no network call)            |
| `Mkdir(u)` / `MkdirAll(u)`  | Creates a directory marker / container           |
| `Copy(src, dst)`            | Server-side copy with stream fallback            |
| `Move(src, dst)`            | Copy + delete                                    |
| `Delete(src)`               | Delete blob or recursive prefix delete           |
| `List(u)`                   | Direct children (hierarchical listing)           |
| `ListIter(ctx, u)`          | Lazily paginated direct children                 |
| `Walk(u, fn)`               | Flat traversal of every blob under the prefix    |
| `Find(u, filter)`           | Find blobs matching a filter                     |
| `DeleteMatching(u, filter)` | Delete blobs matching a filter                   |
| `*Ctx(ctx, ...)`            | Context-aware variants of the above              |

### BlobFile (VFile, RangeReader)

| Method                    | Description                                          |
| ------------------------- | ---------------------------------------------------- |
| `Read(b)`                 | Streams blob content from the current offset         |
| `Write(b)` / `WriteString(s)` | Buffers data (uploaded on Close)                 |
| `Seek(offset, whence)`    | Any whence for reads; no-op only during writes       |
| `ReadRange(ctx, off, n)`  | Ranged GET (`n == 0` → to EOF)                       |
| `Close()`                 | Uploads buffered writes, closes readers              |
| `ListAll()`               | All blobs under this prefix                          |
| `Delete()` / `DeleteAll()`| Delete this blob / everything under this prefix      |
| `Info()`                  | Returns `*BlobFileInfo`                              |
| `Parent()`                | Parent prefix as `VFile`                             |
| `Url()` / `String()`      | The `azblob://` URL                                  |
| `ContentType()`           | MIME type (`application/octet-stream` if unknown)    |
| `AddProperty(k, v)` / `GetProperty(k)` | Blob user metadata                      |
| `AsString()` / `AsBytes()`| Reads entire content (deprecated; use `vfs.ReadAllString` / `vfs.ReadAllBytes`) |

### BlobFileInfo (VFileInfo)

| Method          | Description                                  |
| --------------- | -------------------------------------------- |
| `Name()`        | Blob name                                    |
| `Size()`        | Size in bytes                                |
| `Mode()`        | `os.ModeDir` for directories, else `0`       |
| `ModTime()`     | Last modified time                           |
| `IsDir()`       | `true` for prefixes / markers / container root |
| `ContentType()` | Blob Content-Type                            |
| `ETag()`        | Blob ETag                                    |
| `Sys()`         | The `VFileSystem`                            |

## Testing

Unit tests run fully offline against [`blobtest`](blobtest/), an in-memory fake of the Blob REST API served by `httptest` (containers, Put Blob/Block/Block List, ranged Get, properties, metadata, Copy Blob, List Blobs with pagination, If-Match / If-None-Match, error injection). It is exported so your own tests can use it:

```go
srv := blobtest.NewServer()
defer srv.Close()
srv.CreateContainer("data")
cfg := azcfg.NewConfig()
cfg.SetConnectionString(srv.ConnectionString())
azcfg.Manager.Register(blobtest.AccountName, cfg)
// azblob://devstoreaccount1/data/... now hits the fake
```

Emulator tests run against Azurite when `AZURITE_BLOB_ENDPOINT` is set:

```bash
AZURITE_BLOB_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1 go test ./blob/...
```

## Prerequisites

### Azure RBAC

With Entra ID (token) auth, assign a data-plane role on the storage account or container:

| Role                              | Allows                                                        |
| --------------------------------- | ------------------------------------------------------------- |
| `Storage Blob Data Reader`        | `Read`, `Info`, `List`, `Walk`, `Find`, `GetProperty`         |
| `Storage Blob Data Contributor`   | everything above plus `Create`, `Write`, `Mkdir`, `Copy`, `Move`, `Delete`, `AddProperty`, container creation |

Shared key and connection strings grant full account access; SAS tokens grant whatever permissions they were signed with.

### Credentials

`DefaultAzureCredential` (the default) tries, in order: environment variables (`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, ...), workload identity, managed identity, Azure CLI and Azure Developer CLI logins. See [`azcfg`](../azcfg/) for explicit options.

## Contributing

We welcome contributions. If you find a bug or would like to request a new feature, please open an issue on [GitHub](https://github.com/nandlabs/golly-azure/issues).
