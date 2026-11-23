package servicebus

import (
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

// Option keys for Service Bus-specific options.
const (
	// --- receive options ---

	// OptWaitTimeSeconds is how long (int seconds) Receive / ReceiveBatch and
	// each listener poll wait for the first message. Default: 5.
	OptWaitTimeSeconds = "WaitTimeSeconds"
	// OptTimeout is the overall listener lifetime in seconds. Default: none
	// (the listener runs until removed, its ctx is cancelled or the
	// provider is closed).
	OptTimeout = "Timeout"
	// OptBatchSize is the maximum number of messages returned by
	// ReceiveBatch. Default: 10.
	OptBatchSize = "BatchSize"
	// OptMaxMessages is the maximum number of messages fetched per listener
	// poll (and the ReceiveBatch size when OptBatchSize is not set).
	// Default: 10.
	OptMaxMessages = "MaxMessages"
	// OptReceiveMode selects the settlement mode: "PeekLock" (default) or
	// "ReceiveAndDelete". Accepts a string or azservicebus.ReceiveMode.
	OptReceiveMode = "ReceiveMode"
	// OptSubscription names the topic subscription to receive from. It is
	// an alternative to the ?subscription= URL query parameter.
	OptSubscription = "Subscription"
	// OptAcceptNextSession (bool) receives from the next available session
	// of a session-enabled queue / subscription.
	OptAcceptNextSession = "AcceptNextSession"
	// OptSubQueue selects a sub-queue to receive from: "deadletter" or
	// "transfer". Alternative to the /$deadletterqueue URL suffix.
	OptSubQueue = "SubQueue"

	// --- send + receive options ---

	// OptSessionID (string). On send it sets Message.SessionID explicitly
	// (overriding the Keyed routing key). On receive it locks that specific
	// session of a session-enabled entity.
	OptSessionID = "SessionID"

	// --- send options ---

	// OptPartitionKey (string) sets Message.PartitionKey. For session-aware
	// messages it must equal the SessionID.
	OptPartitionKey = "PartitionKey"
	// OptKeyMapping selects what a messaging.Keyed routing key maps to:
	// KeyMappingSession (default), KeyMappingPartitionKey or KeyMappingNone.
	OptKeyMapping = "KeyMapping"
	// OptMessageID (string) sets Message.MessageID, which Service Bus uses
	// for duplicate detection. Default: the golly message Id(). Single
	// sends only; batches always use each message's Id().
	OptMessageID = "MessageID"
	// OptCorrelationID (string) sets Message.CorrelationID.
	OptCorrelationID = "CorrelationID"
	// OptSubject (string) sets Message.Subject (the "label").
	OptSubject = "Subject"
	// OptContentType (string) sets Message.ContentType.
	OptContentType = "ContentType"
	// OptReplyTo (string) sets Message.ReplyTo.
	OptReplyTo = "ReplyTo"
	// OptTimeToLive sets Message.TimeToLive. Accepts time.Duration or int
	// seconds.
	OptTimeToLive = "TimeToLive"
	// OptScheduledEnqueueTime (time.Time) sets Message.ScheduledEnqueueTime:
	// the message becomes visible to receivers at that time.
	OptScheduledEnqueueTime = "ScheduledEnqueueTime"
	// OptDelaySeconds (int) is a convenience for OptScheduledEnqueueTime =
	// now + n seconds (parity with the sqs provider).
	OptDelaySeconds = "DelaySeconds"

	// --- Rsvp options ---

	// OptDeadLetterReason (string), passed to Rsvp(false, ...), dead-letters
	// the message with the given reason instead of abandoning it.
	OptDeadLetterReason = "DeadLetterReason"
	// OptDeadLetterDescription (string), passed to Rsvp(false, ...), sets
	// the dead-letter error description (implies dead-lettering).
	OptDeadLetterDescription = "DeadLetterDescription"
	// OptDefer (bool), passed to Rsvp(false, ...), defers the message
	// instead of abandoning it.
	OptDefer = "Defer"
)

// KeyMapping values for OptKeyMapping.
const (
	// KeyMappingSession maps the Keyed routing key to Message.SessionID
	// (default). On session-enabled entities this gives FIFO ordering per
	// key; non-session entities accept and ignore the session id.
	KeyMappingSession = "session"
	// KeyMappingPartitionKey maps the routing key to Message.PartitionKey.
	KeyMappingPartitionKey = "partition"
	// KeyMappingNone ignores the routing key.
	KeyMappingNone = "none"
)

// Receive mode values accepted by OptReceiveMode.
const (
	ReceiveModePeekLock         = "PeekLock"
	ReceiveModeReceiveAndDelete = "ReceiveAndDelete"
)

const (
	defaultWaitTime    = 5 * time.Second
	defaultBatchSize   = 10
	maxReceiveMessages = 5000
)

// sendSettings is the parsed set of send-side options.
type sendSettings struct {
	sessionID     string
	hasSessionID  bool
	partitionKey  string
	keyMapping    string
	messageID     string
	correlationID string
	subject       string
	contentType   string
	replyTo       string
	ttl           time.Duration
	hasTTL        bool
	scheduled     time.Time
	hasScheduled  bool
}

func parseSendSettings(r *messaging.OptionsResolver) (*sendSettings, error) {
	s := &sendSettings{keyMapping: KeyMappingSession}
	var err error
	if s.sessionID, s.hasSessionID, err = optString(r, OptSessionID); err != nil {
		return nil, err
	}
	if s.partitionKey, _, err = optString(r, OptPartitionKey); err != nil {
		return nil, err
	}
	if km, ok, err := optString(r, OptKeyMapping); err != nil {
		return nil, err
	} else if ok {
		switch strings.ToLower(km) {
		case KeyMappingSession, "sessionid":
			s.keyMapping = KeyMappingSession
		case KeyMappingPartitionKey, "partitionkey":
			s.keyMapping = KeyMappingPartitionKey
		case KeyMappingNone, "":
			s.keyMapping = KeyMappingNone
		default:
			return nil, fmt.Errorf("%w: %s=%q (want %q, %q or %q)", ErrInvalidOption, OptKeyMapping, km, KeyMappingSession, KeyMappingPartitionKey, KeyMappingNone)
		}
	}
	for key, dst := range map[string]*string{
		OptMessageID:     &s.messageID,
		OptCorrelationID: &s.correlationID,
		OptSubject:       &s.subject,
		OptContentType:   &s.contentType,
		OptReplyTo:       &s.replyTo,
	} {
		if *dst, _, err = optString(r, key); err != nil {
			return nil, err
		}
	}
	if s.ttl, s.hasTTL, err = optDuration(r, OptTimeToLive); err != nil {
		return nil, err
	}
	if s.hasTTL && s.ttl <= 0 {
		return nil, fmt.Errorf("%w: %s must be > 0, got %s", ErrInvalidOption, OptTimeToLive, s.ttl)
	}
	if v, ok := r.Get(OptScheduledEnqueueTime); ok {
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("%w: %s: expected time.Time, got %T", ErrInvalidOption, OptScheduledEnqueueTime, v)
		}
		s.scheduled, s.hasScheduled = t, true
	}
	if n, ok, err := optInt(r, OptDelaySeconds); err != nil {
		return nil, err
	} else if ok {
		if n < 0 {
			return nil, fmt.Errorf("%w: %s must be >= 0, got %d", ErrInvalidOption, OptDelaySeconds, n)
		}
		if s.hasScheduled {
			return nil, fmt.Errorf("%w: %s and %s are mutually exclusive", ErrInvalidOption, OptDelaySeconds, OptScheduledEnqueueTime)
		}
		if n > 0 {
			s.scheduled, s.hasScheduled = time.Now().Add(time.Duration(n)*time.Second), true
		}
	}
	return s, nil
}

// receiveSettings is the parsed set of receive-side options.
type receiveSettings struct {
	mode              azservicebus.ReceiveMode
	hasMode           bool
	wait              time.Duration
	batchSize         int
	maxMessages       int
	timeout           time.Duration
	hasTimeout        bool
	sessionID         string
	acceptNextSession bool
	listenerName      string
}

func (rs *receiveSettings) sessionful() bool {
	return rs.sessionID != "" || rs.acceptNextSession
}

// parseReceiveSettings parses receive options and applies the
// URL-overriding ones (OptSubscription, OptSubQueue) to t.
func parseReceiveSettings(r *messaging.OptionsResolver, t *target) (*receiveSettings, error) {
	rs := &receiveSettings{
		mode:        azservicebus.ReceiveModePeekLock,
		wait:        defaultWaitTime,
		batchSize:   defaultBatchSize,
		maxMessages: defaultBatchSize,
	}
	if v, ok := r.Get(OptReceiveMode); ok {
		m, err := toReceiveMode(v)
		if err != nil {
			return nil, err
		}
		rs.mode, rs.hasMode = m, true
	}
	if n, ok, err := optInt(r, OptWaitTimeSeconds); err != nil {
		return nil, err
	} else if ok {
		if n <= 0 {
			return nil, fmt.Errorf("%w: %s must be > 0, got %d", ErrInvalidOption, OptWaitTimeSeconds, n)
		}
		rs.wait = time.Duration(n) * time.Second
	}
	if n, ok, err := optInt(r, OptMaxMessages); err != nil {
		return nil, err
	} else if ok {
		rs.maxMessages = clamp(n, 1, maxReceiveMessages)
		rs.batchSize = rs.maxMessages
	}
	if n, ok, err := optInt(r, OptBatchSize); err != nil {
		return nil, err
	} else if ok {
		rs.batchSize = clamp(n, 1, maxReceiveMessages)
	}
	if n, ok, err := optInt(r, OptTimeout); err != nil {
		return nil, err
	} else if ok {
		if n <= 0 {
			return nil, fmt.Errorf("%w: %s must be > 0, got %d", ErrInvalidOption, OptTimeout, n)
		}
		rs.timeout, rs.hasTimeout = time.Duration(n)*time.Second, true
	}
	var err error
	if rs.sessionID, _, err = optString(r, OptSessionID); err != nil {
		return nil, err
	}
	if rs.acceptNextSession, _, err = optBool(r, OptAcceptNextSession); err != nil {
		return nil, err
	}
	if rs.sessionID != "" && rs.acceptNextSession {
		return nil, fmt.Errorf("%w: %s and %s are mutually exclusive", ErrInvalidOption, OptSessionID, OptAcceptNextSession)
	}
	if sub, ok, err := optString(r, OptSubscription); err != nil {
		return nil, err
	} else if ok && sub != "" {
		if t.subscription != "" && t.subscription != sub {
			return nil, fmt.Errorf("%w: %s=%q conflicts with URL subscription %q", ErrInvalidOption, OptSubscription, sub, t.subscription)
		}
		t.subscription = sub
	}
	if sq, ok, err := optString(r, OptSubQueue); err != nil {
		return nil, err
	} else if ok {
		switch strings.ToLower(sq) {
		case "deadletter", "dlq", deadLetterSegment:
			t.subQueue = azservicebus.SubQueueDeadLetter
		case "transfer", "transferdeadletter", transferDeadLetterSegment:
			t.subQueue = azservicebus.SubQueueTransfer
		case "", "none":
			t.subQueue = 0
		default:
			return nil, fmt.Errorf("%w: %s=%q (want \"deadletter\" or \"transfer\")", ErrInvalidOption, OptSubQueue, sq)
		}
	}
	if t.subQueue != 0 && rs.sessionful() {
		return nil, fmt.Errorf("%w: sessions cannot be used with a dead-letter sub-queue", ErrInvalidOption)
	}
	if v, ok := messaging.ResolveOptValue[string](messaging.NamedListener, r); ok {
		rs.listenerName = v
	}
	return rs, nil
}

func toReceiveMode(v any) (azservicebus.ReceiveMode, error) {
	switch m := v.(type) {
	case azservicebus.ReceiveMode:
		if m != azservicebus.ReceiveModePeekLock && m != azservicebus.ReceiveModeReceiveAndDelete {
			return 0, fmt.Errorf("%w: unknown %s %d", ErrInvalidOption, OptReceiveMode, m)
		}
		return m, nil
	case string:
		switch strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(m)) {
		case "peeklock":
			return azservicebus.ReceiveModePeekLock, nil
		case "receiveanddelete":
			return azservicebus.ReceiveModeReceiveAndDelete, nil
		}
		return 0, fmt.Errorf("%w: %s=%q (want %q or %q)", ErrInvalidOption, OptReceiveMode, m, ReceiveModePeekLock, ReceiveModeReceiveAndDelete)
	default:
		return 0, fmt.Errorf("%w: %s: expected string or azservicebus.ReceiveMode, got %T", ErrInvalidOption, OptReceiveMode, v)
	}
}

// --- typed option helpers -----------------------------------------------

func optString(r *messaging.OptionsResolver, key string) (string, bool, error) {
	v, ok := r.Get(key)
	if !ok {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("%w: %s: expected string, got %T", ErrInvalidOption, key, v)
	}
	return s, true, nil
}

func optBool(r *messaging.OptionsResolver, key string) (bool, bool, error) {
	v, ok := r.Get(key)
	if !ok {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, fmt.Errorf("%w: %s: expected bool, got %T", ErrInvalidOption, key, v)
	}
	return b, true, nil
}

func optInt(r *messaging.OptionsResolver, key string) (int, bool, error) {
	v, ok := r.Get(key)
	if !ok {
		return 0, false, nil
	}
	switch n := v.(type) {
	case int:
		return n, true, nil
	case int32:
		return int(n), true, nil
	case int64:
		return int(n), true, nil
	default:
		return 0, false, fmt.Errorf("%w: %s: expected int, got %T", ErrInvalidOption, key, v)
	}
}

// optDuration accepts a time.Duration or an int number of seconds.
func optDuration(r *messaging.OptionsResolver, key string) (time.Duration, bool, error) {
	v, ok := r.Get(key)
	if !ok {
		return 0, false, nil
	}
	switch d := v.(type) {
	case time.Duration:
		return d, true, nil
	case int:
		return time.Duration(d) * time.Second, true, nil
	case int64:
		return time.Duration(d) * time.Second, true, nil
	default:
		return 0, false, fmt.Errorf("%w: %s: expected time.Duration or int seconds, got %T", ErrInvalidOption, key, v)
	}
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
