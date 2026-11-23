package servicebus

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

// maxLockDuration is the Service Bus maximum message / session lock
// duration.
const maxLockDuration = 5 * time.Minute

// brokerOptions holds the Service Bus translation of the broker-targeted
// messaging options (see oss.nandlabs.io/golly/messaging/broker_options.go).
//
// Constructed by parseBrokerOptions; the has* booleans distinguish "unset"
// from "zero".
type brokerOptions struct {
	// receiveMode is derived from DeliveryGuarantee: AtLeastOnce →
	// PeekLock, AtMostOnce → ReceiveAndDelete.
	receiveMode    azservicebus.ReceiveMode
	hasReceiveMode bool

	// exactlyOnce is set for DeliveryGuarantee=ExactlyOnce. Only accepted on
	// the send path, where it is verified against the destination's
	// RequiresDuplicateDetection setting.
	exactlyOnce bool

	// maxDeliveryAttempts is the caller's minimum acceptable entity
	// MaxDeliveryCount.
	maxDeliveryAttempts    int
	hasMaxDeliveryAttempts bool

	// deadLetter is the requested DLQ. The built-in "$deadletterqueue"
	// always exists; any other value must match the entity's
	// ForwardDeadLetteredMessagesTo.
	deadLetter string

	// ackTimeout must not exceed the entity's LockDuration.
	ackTimeout    time.Duration
	hasAckTimeout bool
}

// parseBrokerOptions extracts and validates the broker-targeted options.
// forSend selects send-side semantics: on the send path receive-only options
// (ConsumerMode, AckTimeout, DeadLetter, MaxDeliveryAttempts) are rejected,
// because they are properties of the queue / subscription, not the sender.
//
//   - DeliveryGuarantee: AtLeastOnce → PeekLock (default). AtMostOnce →
//     ReceiveAndDelete on receive; accepted with a warning on send.
//     ExactlyOnce → send only; requires duplicate detection on the entity.
//   - ConsumerMode: Pull accepted (the Go SDK is pull-based); Push rejected.
//   - AckTimeout: must be > 0 and <= 5m; verified against LockDuration.
//   - MaxDeliveryAttempts: verified against MaxDeliveryCount (>=).
//   - DeadLetter: "$deadletterqueue" (built-in) or the entity's
//     ForwardDeadLetteredMessagesTo target.
func parseBrokerOptions(r *messaging.OptionsResolver, forSend bool) (*brokerOptions, error) {
	bo := &brokerOptions{}
	if r == nil {
		return bo, nil
	}

	if v, ok := r.Get(messaging.DeliveryGuaranteeOpt); ok {
		g, ok := v.(messaging.DeliveryGuarantee)
		if !ok {
			return nil, fmt.Errorf("%w: %s: expected messaging.DeliveryGuarantee, got %T", ErrInvalidOption, messaging.DeliveryGuaranteeOpt, v)
		}
		switch g {
		case messaging.AtLeastOnce:
			bo.receiveMode, bo.hasReceiveMode = azservicebus.ReceiveModePeekLock, !forSend
		case messaging.AtMostOnce:
			if forSend {
				logger.WarnF("servicebus: DeliveryGuarantee=%q has no sender-side effect; receive with ReceiveAndDelete for at-most-once delivery", g)
			} else {
				bo.receiveMode, bo.hasReceiveMode = azservicebus.ReceiveModeReceiveAndDelete, true
			}
		case messaging.ExactlyOnce:
			if !forSend {
				return nil, fmt.Errorf("%w: DeliveryGuarantee=%q is not supported for receivers; Service Bus consumers are at-least-once (PeekLock) — make handlers idempotent", ErrUnsupportedOption, g)
			}
			bo.exactlyOnce = true
		default:
			return nil, fmt.Errorf("%w: DeliveryGuarantee=%q is not recognized", ErrInvalidOption, g)
		}
	}

	if v, ok := r.Get(messaging.ConsumerModeOpt); ok {
		m, ok := v.(messaging.ConsumerMode)
		if !ok {
			return nil, fmt.Errorf("%w: %s: expected messaging.ConsumerMode, got %T", ErrInvalidOption, messaging.ConsumerModeOpt, v)
		}
		switch {
		case forSend:
			return nil, fmt.Errorf("%w: ConsumerMode is a receive-side option", ErrUnsupportedOption)
		case m == messaging.ConsumerPull:
			// The Go SDK's ReceiveMessages is pull-based — accept.
		case m == messaging.ConsumerPush:
			return nil, fmt.Errorf("%w: ConsumerMode=%q is not supported; the Service Bus Go SDK is pull-based (listeners poll ReceiveMessages)", ErrUnsupportedOption, m)
		default:
			return nil, fmt.Errorf("%w: ConsumerMode=%q is not recognized", ErrInvalidOption, m)
		}
	}

	if v, ok := r.Get(messaging.AckTimeoutOpt); ok {
		d, ok := v.(time.Duration)
		if !ok {
			return nil, fmt.Errorf("%w: %s: expected time.Duration, got %T", ErrInvalidOption, messaging.AckTimeoutOpt, v)
		}
		if forSend {
			return nil, fmt.Errorf("%w: AckTimeout is a receive-side option (the entity's LockDuration)", ErrUnsupportedOption)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%w: AckTimeout must be > 0, got %s", ErrInvalidOption, d)
		}
		if d > maxLockDuration {
			return nil, fmt.Errorf("%w: AckTimeout %s exceeds the Service Bus maximum lock duration of %s", ErrUnsupportedOption, d, maxLockDuration)
		}
		bo.ackTimeout, bo.hasAckTimeout = d, true
	}

	if v, ok := r.Get(messaging.MaxDeliveryAttemptsOpt); ok {
		n, ok := v.(int)
		if !ok {
			return nil, fmt.Errorf("%w: %s: expected int, got %T", ErrInvalidOption, messaging.MaxDeliveryAttemptsOpt, v)
		}
		if forSend {
			return nil, fmt.Errorf("%w: MaxDeliveryAttempts is configured on the queue / subscription (MaxDeliveryCount), not the sender", ErrUnsupportedOption)
		}
		if n <= 0 {
			return nil, fmt.Errorf("%w: MaxDeliveryAttempts must be > 0, got %d", ErrInvalidOption, n)
		}
		bo.maxDeliveryAttempts, bo.hasMaxDeliveryAttempts = n, true
	}

	if v, ok := r.Get(messaging.DeadLetterOpt); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s: expected string, got %T", ErrInvalidOption, messaging.DeadLetterOpt, v)
		}
		if forSend {
			return nil, fmt.Errorf("%w: DeadLetter is configured on the queue / subscription, not the sender", ErrUnsupportedOption)
		}
		if s == "" {
			return nil, fmt.Errorf("%w: DeadLetter must be a non-empty entity name", ErrInvalidOption)
		}
		bo.deadLetter = s
	}

	return bo, nil
}

// isBuiltinDeadLetter reports whether name refers to the built-in
// dead-letter sub-queue, which every queue and subscription has.
func isBuiltinDeadLetter(name string) bool {
	n := strings.ToLower(strings.TrimSuffix(name, "/"))
	return n == deadLetterSegment || strings.HasSuffix(n, "/"+deadLetterSegment)
}

// needsVerification reports whether any option requires a management-plane
// lookup of the entity configuration.
func (bo *brokerOptions) needsVerification() bool {
	return bo.exactlyOnce || bo.hasMaxDeliveryAttempts || bo.hasAckTimeout ||
		(bo.deadLetter != "" && !isBuiltinDeadLetter(bo.deadLetter))
}

// verify fetches the entity configuration through the management plane and
// checks it against the requested options. This provider never creates or
// reconfigures entities; mismatches return ErrEntityMismatch pointing the
// caller at entity creation.
func (bo *brokerOptions) verify(ctx context.Context, u *url.URL, t target, forSend bool) error {
	if !bo.needsVerification() {
		return nil
	}
	adm, err := connectAdmin(u, t)
	if err != nil {
		return err
	}
	info, err := adm.EntityInfo(ctx, t, forSend)
	if err != nil {
		return fmt.Errorf("servicebus: failed to fetch entity configuration for %s: %w", t.entityPath(), err)
	}
	if info == nil {
		return fmt.Errorf("%w: entity %s does not exist", ErrEntityMismatch, t.entityPath())
	}
	return bo.check(info, t)
}

// check validates info against the requested options.
func (bo *brokerOptions) check(info *entityInfo, t target) error {
	path := t.entityPath()
	if bo.exactlyOnce && !info.requiresDuplicateDetection {
		return fmt.Errorf("%w: DeliveryGuarantee=exactly-once requires duplicate detection (RequiresDuplicateDetection) on %s; enable it at entity creation time", ErrEntityMismatch, t.entity)
	}
	if bo.hasMaxDeliveryAttempts {
		if !info.hasMaxDeliveryCount {
			return fmt.Errorf("%w: %s reports no MaxDeliveryCount", ErrEntityMismatch, path)
		}
		if int(info.maxDeliveryCount) < bo.maxDeliveryAttempts {
			return fmt.Errorf("%w: %s MaxDeliveryCount=%d is less than requested MaxDeliveryAttempts=%d", ErrEntityMismatch, path, info.maxDeliveryCount, bo.maxDeliveryAttempts)
		}
	}
	if bo.hasAckTimeout && info.hasLockDuration && info.lockDuration < bo.ackTimeout {
		return fmt.Errorf("%w: %s LockDuration=%s is shorter than requested AckTimeout=%s; raise LockDuration on the entity", ErrEntityMismatch, path, info.lockDuration, bo.ackTimeout)
	}
	if bo.deadLetter != "" && !isBuiltinDeadLetter(bo.deadLetter) {
		if !forwardTargetMatches(info.forwardDeadLetteredTo, bo.deadLetter) {
			return fmt.Errorf("%w: %s ForwardDeadLetteredMessagesTo=%q does not match requested DeadLetter=%q (use %q for the built-in dead-letter queue)", ErrEntityMismatch, path, info.forwardDeadLetteredTo, bo.deadLetter, deadLetterSegment)
		}
	}
	return nil
}

// forwardTargetMatches compares the entity's ForwardDeadLetteredMessagesTo
// (often an absolute URI such as "sb://ns.servicebus.windows.net/dlq")
// against the requested name, case-insensitively.
func forwardTargetMatches(actual, want string) bool {
	if actual == "" {
		return false
	}
	a, w := strings.ToLower(strings.TrimSuffix(actual, "/")), strings.ToLower(strings.TrimSuffix(want, "/"))
	if a == w {
		return true
	}
	if pu, err := url.Parse(a); err == nil && pu.Host != "" {
		a = strings.TrimPrefix(pu.Path, "/")
	}
	return a == strings.TrimPrefix(w, "/")
}
