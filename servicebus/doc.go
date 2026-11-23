// Package servicebus provides an Azure Service Bus implementation of the
// golly messaging.Provider interface, covering both queues and
// topics / subscriptions.
//
// It uses github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus and
// the standard golly messaging URL scheme:
//
//   - Queue (send + receive):  servicebus://<namespace>/<queue>
//   - Topic (send):            servicebus://<namespace>/<topic>
//   - Subscription (receive):  servicebus://<namespace>/<topic>?subscription=<sub>
//     or the Azure entity path servicebus://<namespace>/<topic>/subscriptions/<sub>
//   - Dead-letter sub-queue:   append /$deadletterqueue to a queue or subscription path
//
// <namespace> is either a bare namespace name ("myns", expanded to
// myns.servicebus.windows.net or the sovereign-cloud equivalent) or a fully
// qualified host. Configuration is resolved through azcfg: the namespace,
// then the "servicebus" fallback name, then "default". A configured
// ConnectionString is used when present, otherwise the azcfg token
// credential (DefaultAzureCredential by default).
//
// Keyed messages (messaging.Keyed) map their routing key to the Service Bus
// SessionID by default (OptKeyMapping switches to PartitionKey or none).
// Received messages expose the SessionID (or PartitionKey) as their routing
// key. golly headers travel as Service Bus application properties with their
// native AMQP types.
//
// Received messages are PeekLock-ed by default: Rsvp(true) completes,
// Rsvp(false) abandons (redelivery), Rsvp(false, Requeue=false) or a
// DeadLetterReason dead-letters, and Defer=true defers.
//
// The provider auto-registers with the golly messaging manager on import.
//
// Usage:
//
//	import _ "oss.nandlabs.io/golly-azure/servicebus"
//
//	mgr := messaging.GetManager()
//	msg, _ := mgr.NewMessage("servicebus")
//	msg.SetBodyStr("hello world")
//	u, _ := url.Parse("servicebus://my-namespace/orders")
//	mgr.Send(u, msg)
package servicebus
