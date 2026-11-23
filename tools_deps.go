//go:build tools

// Package tools pins dependencies used across golly-azure packages while
// they are being developed. Removed before merge.
package tools

import (
	_ "github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	_ "github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	_ "github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	_ "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	_ "github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	_ "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	_ "github.com/alicebob/miniredis/v2"
	_ "github.com/openai/openai-go/v3"
	_ "github.com/redis/go-redis/v9"
)
