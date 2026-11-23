package servicebus

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly-azure/azcfg"
)

const (
	// ServiceBusScheme is the URL scheme for Azure Service Bus.
	ServiceBusScheme = "servicebus"
	// ServiceBusProviderID is the provider identifier.
	ServiceBusProviderID = "servicebus-provider"

	// SubscriptionQueryParam is the URL query parameter naming the topic
	// subscription to receive from: servicebus://<ns>/<topic>?subscription=<sub>.
	SubscriptionQueryParam = "subscription"

	// subscriptionsSegment is the Azure entity-path segment separating a
	// topic from its subscription: <topic>/subscriptions/<sub>.
	subscriptionsSegment = "subscriptions"
	// deadLetterSegment is the Azure entity-path suffix of the built-in
	// dead-letter sub-queue: <queue>/$deadletterqueue.
	deadLetterSegment = "$deadletterqueue"
	// transferDeadLetterSegment is the Azure entity-path suffix of the
	// transfer dead-letter sub-queue.
	transferDeadLetterSegment = "$transferdeadletterqueue"

	// Default DNS suffixes for Service Bus namespaces per Azure cloud.
	publicSuffix = "servicebus.windows.net"
	govSuffix    = "servicebus.usgovcloudapi.net"
	chinaSuffix  = "servicebus.chinacloudapi.cn"
)

var (
	// ErrInvalidURL is returned when a servicebus:// URL is missing the
	// namespace (host) or the entity (queue / topic) path.
	ErrInvalidURL = errors.New("servicebus: invalid URL")
	// ErrNoMessages is returned by Receive / ReceiveBatch when no message
	// arrived within the wait time.
	ErrNoMessages = errors.New("servicebus: no messages available")
	// ErrInvalidOption is returned (wrapped) when an option has the wrong
	// type or an out-of-range value, or when options conflict.
	ErrInvalidOption = errors.New("servicebus: invalid option")
	// ErrUnsupportedOption is returned (wrapped) when a broker-targeted
	// option asks for semantics Service Bus cannot honour.
	ErrUnsupportedOption = errors.New("servicebus: unsupported option")
	// ErrEntityMismatch is returned (wrapped) when the server-side entity
	// configuration does not satisfy a broker-targeted option
	// (MaxDeliveryAttempts, DeadLetter, AckTimeout, ExactlyOnce).
	ErrEntityMismatch = errors.New("servicebus: entity configuration mismatch")
)

// target is a parsed servicebus:// URL.
type target struct {
	// namespace is the URL host: a bare namespace name ("myns") or a
	// fully-qualified host ("myns.servicebus.windows.net", "localhost").
	namespace string
	// entity is the queue or topic name.
	entity string
	// subscription is the topic subscription name; empty for queues.
	subscription string
	// subQueue selects the dead-letter / transfer dead-letter sub-queue.
	subQueue azservicebus.SubQueue
}

// parseTarget parses a servicebus:// URL. Supported forms:
//
//	servicebus://<namespace>/<queue>
//	servicebus://<namespace>/<topic>?subscription=<subscription>
//	servicebus://<namespace>/<topic>/subscriptions/<subscription>
//	servicebus://<namespace>/<queue>/$deadletterqueue
//	servicebus://<namespace>/<topic>/subscriptions/<sub>/$deadletterqueue
func parseTarget(u *url.URL) (target, error) {
	var t target
	if u == nil {
		return t, fmt.Errorf("%w: nil URL", ErrInvalidURL)
	}
	t.namespace = u.Host
	if t.namespace == "" {
		return t, fmt.Errorf("%w: namespace (URL host) is required in %q", ErrInvalidURL, u.String())
	}
	segs := splitPath(u.Path)
	if n := len(segs); n > 0 {
		switch strings.ToLower(segs[n-1]) {
		case deadLetterSegment:
			t.subQueue = azservicebus.SubQueueDeadLetter
			segs = segs[:n-1]
		case transferDeadLetterSegment:
			t.subQueue = azservicebus.SubQueueTransfer
			segs = segs[:n-1]
		}
	}
	switch {
	case len(segs) == 1:
		t.entity = segs[0]
	case len(segs) == 3 && strings.EqualFold(segs[1], subscriptionsSegment):
		t.entity, t.subscription = segs[0], segs[2]
	case len(segs) == 0:
		return t, fmt.Errorf("%w: queue or topic name (URL path) is required in %q", ErrInvalidURL, u.String())
	default:
		return t, fmt.Errorf("%w: unsupported entity path %q (expected /<queue>, /<topic>?subscription=<sub> or /<topic>/subscriptions/<sub>)", ErrInvalidURL, u.Path)
	}
	if sub := u.Query().Get(SubscriptionQueryParam); sub != "" {
		if t.subscription != "" && t.subscription != sub {
			return t, fmt.Errorf("%w: subscription given in both path (%q) and query (%q)", ErrInvalidURL, t.subscription, sub)
		}
		t.subscription = sub
	}
	return t, nil
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// key identifies the receive source (namespace + entity [+ subscription]
// [+ sub-queue]); it is used to group listeners for RemoveListeners.
func (t target) key() string {
	k := t.namespace + "/" + t.entity
	if t.subscription != "" {
		k += "/" + subscriptionsSegment + "/" + t.subscription
	}
	switch t.subQueue {
	case azservicebus.SubQueueDeadLetter:
		k += "/" + deadLetterSegment
	case azservicebus.SubQueueTransfer:
		k += "/" + transferDeadLetterSegment
	}
	return k
}

// senderKey identifies the send destination (namespace + queue/topic).
func (t target) senderKey() string {
	return t.namespace + "/" + t.entity
}

// entityPath returns the Azure entity path of the receive source.
func (t target) entityPath() string {
	if t.subscription != "" {
		return t.entity + "/" + subscriptionsSegment + "/" + t.subscription
	}
	return t.entity
}

// resolveConfig returns the azcfg.Config for the URL. Lookup order: the
// namespace (URL host), host+path, the "servicebus" fallback name, the
// "default" config, and finally a fresh DefaultAzureCredential config.
func resolveConfig(u *url.URL) *azcfg.Config {
	return azcfg.GetConfigOrDefault(u, ServiceBusScheme)
}

// fullyQualifiedNamespace expands a bare namespace name into its
// fully-qualified host for the configured cloud. Hosts that already
// contain a dot (or "localhost") are returned unchanged.
func fullyQualifiedNamespace(ns string, cfg *azcfg.Config) string {
	if strings.Contains(ns, ".") || strings.HasPrefix(ns, "localhost") {
		return ns
	}
	suffix := publicSuffix
	if cfg != nil {
		authority := strings.ToLower(cfg.Cloud.ActiveDirectoryAuthorityHost)
		switch {
		case strings.Contains(authority, "microsoftonline.us"):
			suffix = govSuffix
		case strings.Contains(authority, "chinacloudapi.cn"):
			suffix = chinaSuffix
		}
	}
	return ns + "." + suffix
}
