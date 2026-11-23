package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"oss.nandlabs.io/golly-azure/azcfg"
	_ "oss.nandlabs.io/golly-azure/blob"
	"oss.nandlabs.io/golly/vfs"
)

// azuriteKey is the well-known Azurite development account key.
const azuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// Run against Azurite:
//
//	docker run -p 10000:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
//	AZBLOB_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1 go run ./examples/blob
//
// Run against Azure (DefaultAzureCredential, e.g. after `az login`):
//
//	AZURE_STORAGE_ACCOUNT=mystorageacct go run ./examples/blob
//
// AZURE_STORAGE_CONNECTION_STRING or AZURE_STORAGE_KEY switch to
// connection-string or shared-key auth.
func main() {
	account := envOrDefault("AZURE_STORAGE_ACCOUNT", "devstoreaccount1")
	containerName := envOrDefault("CONTAINER_NAME", "golly-blobvfs-example")
	endpoint := os.Getenv("AZBLOB_ENDPOINT")

	// Configure Azure
	cfg := azcfg.NewConfig()
	switch {
	case os.Getenv("AZURE_STORAGE_CONNECTION_STRING") != "":
		cfg.SetConnectionString(os.Getenv("AZURE_STORAGE_CONNECTION_STRING"))
		fmt.Println("Using connection string auth")
	case endpoint != "":
		cfg.SetEndpoint(endpoint)
		cfg.SetSharedKey(account, envOrDefault("AZURE_STORAGE_KEY", azuriteKey))
		fmt.Printf("Using custom endpoint: %s\n", endpoint)
	case os.Getenv("AZURE_STORAGE_KEY") != "":
		cfg.SetSharedKey(account, os.Getenv("AZURE_STORAGE_KEY"))
		fmt.Println("Using shared key auth")
	default:
		fmt.Println("Using DefaultAzureCredential")
	}
	azcfg.Manager.Register(account, cfg)

	mgr := vfs.GetManager()
	root := fmt.Sprintf("azblob://%s/%s", account, containerName)
	base := root + "/blobvfs-demo"

	fmt.Println("=== Azure blob VFS Comprehensive Example ===")
	fmt.Printf("Account: %s | Container: %s\n\n", account, containerName)

	// 0. Ensure the container exists (Mkdir on a container root creates it)
	step("0. Ensure container")
	c, err := mgr.MkdirAllRaw(root)
	check(err, "MkdirAllRaw container")
	_ = c.Close()

	// 1. Create a directory
	step("1. Create directory")
	dir, err := mgr.MkdirRaw(base + "/data/")
	check(err, "MkdirRaw")
	_ = dir.Close()

	// 2. Write files
	step("2. Write files")
	writeFile(mgr, base+"/data/hello.txt", "Hello from golly blob VFS!")
	writeFile(mgr, base+"/data/config.json", `{"app":"golly","version":"1.0"}`)
	writeFile(mgr, base+"/data/notes.md", "# Notes\n\nA markdown file.")
	writeFile(mgr, base+"/data/reports/summary.csv", "name,value\nmetric_a,100\nmetric_b,200")
	writeFile(mgr, base+"/data/reports/detail.csv", "id,name,score\n1,Alice,95\n2,Bob,87")
	writeFile(mgr, base+"/data/logs/app.log", "2026-02-26 INFO Application started")

	// 3. Read file as string
	step("3. Read file as string")
	readAndPrint(mgr, base+"/data/hello.txt")

	// 4. Read file as bytes
	step("4. Read file as bytes")
	f, err := mgr.OpenRaw(base + "/data/config.json")
	check(err, "OpenRaw config.json")
	data, err := vfs.ReadAllBytes(f)
	check(err, "ReadAllBytes")
	_ = f.Close()
	fmt.Printf("  config.json (%d bytes): %s\n", len(data), string(data))

	// 5. File info
	step("5. File info")
	f, err = mgr.OpenRaw(base + "/data/hello.txt")
	check(err, "OpenRaw hello.txt")
	info, err := f.Info()
	check(err, "Info")
	_ = f.Close()
	fmt.Printf("  Name=%s Size=%d ModTime=%s IsDir=%t\n",
		info.Name(), info.Size(), info.ModTime(), info.IsDir())

	// 6. List directory
	step("6. List directory")
	files, err := mgr.ListRaw(base + "/data/")
	check(err, "ListRaw")
	for _, fl := range files {
		fi, _ := fl.Info()
		fmt.Printf("  %s (dir=%t)\n", fi.Name(), fi.IsDir())
	}

	// 7. Walk entire tree
	step("7. Walk tree")
	err = mgr.WalkRaw(base+"/data/", func(file vfs.VFile) error {
		fi, _ := file.Info()
		fmt.Printf("  %s\n", fi.Name())
		return nil
	})
	check(err, "WalkRaw")

	// 8. Find CSV files
	step("8. Find CSV files")
	loc, _ := url.Parse(base + "/data/")
	csvFiles, err := mgr.Find(loc, func(file vfs.VFile) (bool, error) {
		fi, e := file.Info()
		if e != nil {
			return false, e
		}
		return strings.HasSuffix(fi.Name(), ".csv"), nil
	})
	check(err, "Find")
	for _, cf := range csvFiles {
		fmt.Printf("  %s\n", cf.Url().String())
	}

	// 9. Metadata properties
	step("9. Metadata properties")
	f, err = mgr.OpenRaw(base + "/data/hello.txt")
	check(err, "OpenRaw metadata")
	check(f.AddProperty("department", "engineering"), "AddProperty")
	fmt.Println("  Set department=engineering")
	dept, err := f.GetProperty("department")
	check(err, "GetProperty")
	fmt.Printf("  Got department=%s\n", dept)
	_ = f.Close()

	// 10. Copy file
	step("10. Copy file")
	err = mgr.CopyRaw(base+"/data/hello.txt", base+"/backup/hello-copy.txt")
	check(err, "CopyRaw")
	fmt.Println("  Copied hello.txt -> backup/hello-copy.txt")
	readAndPrint(mgr, base+"/backup/hello-copy.txt")

	// 11. Move file
	step("11. Move file")
	err = mgr.MoveRaw(base+"/data/notes.md", base+"/archive/notes.md")
	check(err, "MoveRaw")
	fmt.Println("  Moved notes.md -> archive/notes.md")
	readAndPrint(mgr, base+"/archive/notes.md")

	// 12. Parent navigation
	step("12. Parent navigation")
	f, err = mgr.OpenRaw(base + "/data/reports/summary.csv")
	check(err, "OpenRaw parent")
	parent, err := f.Parent()
	check(err, "Parent")
	fmt.Printf("  Child:  %s\n", f.Url())
	fmt.Printf("  Parent: %s\n", parent.Url())
	_ = f.Close()
	_ = parent.Close()

	// 13. Delete matching (.log files)
	step("13. Delete matching (.log files)")
	logLoc, _ := url.Parse(base + "/data/")
	err = mgr.DeleteMatching(logLoc, func(file vfs.VFile) (bool, error) {
		fi, e := file.Info()
		if e != nil {
			return false, e
		}
		return strings.HasSuffix(fi.Name(), ".log"), nil
	})
	check(err, "DeleteMatching")
	fmt.Println("  Deleted all .log files")

	// 14. Cleanup
	step("14. Cleanup")
	err = mgr.DeleteRaw(base + "/")
	check(err, "DeleteRaw cleanup")
	fmt.Println("  Cleaned up all demo data")

	fmt.Println("\n=== Example complete ===")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func check(err error, ctx string) {
	if err != nil {
		log.Fatalf("%s: %v", ctx, err)
	}
}

func step(name string) {
	fmt.Printf("\n--- %s ---\n", name)
}

func writeFile(mgr vfs.Manager, path, content string) {
	f, err := mgr.CreateRaw(path)
	check(err, "CreateRaw "+path)
	_, err = f.WriteString(content)
	check(err, "WriteString "+path)
	check(f.Close(), "Close "+path)
	fmt.Printf("  Created: %s (%d bytes)\n", path, len(content))
}

func readAndPrint(mgr vfs.Manager, path string) {
	f, err := mgr.OpenRaw(path)
	check(err, "OpenRaw "+path)
	s, err := vfs.ReadAllString(f)
	check(err, "ReadAllString "+path)
	_ = f.Close()
	fmt.Printf("  Content: %q\n", s)
}
