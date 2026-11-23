package servicebus

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

// --- fake batch ---------------------------------------------------------

type fakeBatch struct {
	max  int
	msgs []*azservicebus.Message
}

func (b *fakeBatch) AddMessage(m *azservicebus.Message, _ *azservicebus.AddMessageOptions) error {
	if len(b.msgs) >= b.max || len(m.Body) > 1024 {
		return azservicebus.ErrMessageTooLarge
	}
	b.msgs = append(b.msgs, m)
	return nil
}

func (b *fakeBatch) NumMessages() int32 { return int32(len(b.msgs)) }

// --- fake sender ----------------------------------------------------------

type fakeSender struct {
	mu       sync.Mutex
	entity   string
	sent     []*azservicebus.Message
	batches  [][]*azservicebus.Message
	batchMax int
	sendFn   func(ctx context.Context, m *azservicebus.Message) error
	batchErr error
	newBatch error
	closed   atomic.Bool
}

func (s *fakeSender) SendMessage(ctx context.Context, m *azservicebus.Message, _ *azservicebus.SendMessageOptions) error {
	if s.sendFn != nil {
		if err := s.sendFn(ctx, m); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.sent = append(s.sent, m)
	s.mu.Unlock()
	return nil
}

func (s *fakeSender) NewMessageBatch(context.Context) (messageBatch, error) {
	if s.newBatch != nil {
		return nil, s.newBatch
	}
	max := s.batchMax
	if max == 0 {
		max = 100
	}
	return &fakeBatch{max: max}, nil
}

func (s *fakeSender) SendMessageBatch(_ context.Context, b messageBatch) error {
	if s.batchErr != nil {
		return s.batchErr
	}
	s.mu.Lock()
	s.batches = append(s.batches, b.(*fakeBatch).msgs)
	s.mu.Unlock()
	return nil
}

func (s *fakeSender) Close(context.Context) error { s.closed.Store(true); return nil }

func (s *fakeSender) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// --- fake receiver --------------------------------------------------------

type fakeReceiver struct {
	mu        sync.Mutex
	t         target
	mode      azservicebus.ReceiveMode
	sessionID string
	queue     []*azservicebus.ReceivedMessage
	recvFn    func(ctx context.Context, max int) ([]*azservicebus.ReceivedMessage, error)
	maxSeen   []int

	completed    []*azservicebus.ReceivedMessage
	abandoned    []*azservicebus.ReceivedMessage
	deadLettered []*azservicebus.DeadLetterOptions
	deferred     []*azservicebus.ReceivedMessage
	settleErr    error

	closed atomic.Bool
}

func (r *fakeReceiver) push(msgs ...*azservicebus.ReceivedMessage) {
	r.mu.Lock()
	r.queue = append(r.queue, msgs...)
	r.mu.Unlock()
}

func (r *fakeReceiver) ReceiveMessages(ctx context.Context, max int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
	r.mu.Lock()
	r.maxSeen = append(r.maxSeen, max)
	fn := r.recvFn
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx, max)
	}
	for {
		r.mu.Lock()
		if len(r.queue) > 0 {
			n := max
			if n > len(r.queue) {
				n = len(r.queue)
			}
			out := r.queue[:n]
			r.queue = r.queue[n:]
			r.mu.Unlock()
			return out, nil
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (r *fakeReceiver) CompleteMessage(_ context.Context, m *azservicebus.ReceivedMessage, _ *azservicebus.CompleteMessageOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settleErr != nil {
		return r.settleErr
	}
	r.completed = append(r.completed, m)
	return nil
}

func (r *fakeReceiver) AbandonMessage(_ context.Context, m *azservicebus.ReceivedMessage, _ *azservicebus.AbandonMessageOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settleErr != nil {
		return r.settleErr
	}
	r.abandoned = append(r.abandoned, m)
	return nil
}

func (r *fakeReceiver) DeadLetterMessage(_ context.Context, _ *azservicebus.ReceivedMessage, o *azservicebus.DeadLetterOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settleErr != nil {
		return r.settleErr
	}
	r.deadLettered = append(r.deadLettered, o)
	return nil
}

func (r *fakeReceiver) DeferMessage(_ context.Context, m *azservicebus.ReceivedMessage, _ *azservicebus.DeferMessageOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferred = append(r.deferred, m)
	return nil
}

func (r *fakeReceiver) Close(context.Context) error { r.closed.Store(true); return nil }

func (r *fakeReceiver) counts() (completed, abandoned, deadLettered, deferred int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.completed), len(r.abandoned), len(r.deadLettered), len(r.deferred)
}

// --- fake client ------------------------------------------------------------

type fakeClient struct {
	mu        sync.Mutex
	senders   map[string]*fakeSender
	receivers []*fakeReceiver
	sessions  []*fakeReceiver

	senderErr   error
	receiverErr error
	// newReceiverFn customises receivers returned by NewReceiver.
	newReceiverFn func(t target, mode azservicebus.ReceiveMode) (*fakeReceiver, error)
	// acceptFn customises AcceptSession.
	acceptFn func(ctx context.Context, t target, sessionID string, mode azservicebus.ReceiveMode) (*fakeReceiver, error)

	closed atomic.Bool
}

func newFakeClient() *fakeClient { return &fakeClient{senders: map[string]*fakeSender{}} }

func (c *fakeClient) NewSender(entity string) (sbSender, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.senderErr != nil {
		return nil, c.senderErr
	}
	s := c.senders[entity]
	if s == nil || s.closed.Load() {
		s = &fakeSender{entity: entity}
		c.senders[entity] = s
	}
	return s, nil
}

func (c *fakeClient) sender(entity string) *fakeSender {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.senders[entity]
	if s == nil {
		s = &fakeSender{entity: entity}
		c.senders[entity] = s
	}
	return s
}

func (c *fakeClient) NewReceiver(t target, mode azservicebus.ReceiveMode) (sbReceiver, error) {
	if c.receiverErr != nil {
		return nil, c.receiverErr
	}
	var (
		r   *fakeReceiver
		err error
	)
	if c.newReceiverFn != nil {
		r, err = c.newReceiverFn(t, mode)
		if err != nil {
			return nil, err
		}
	} else {
		r = &fakeReceiver{}
	}
	r.t, r.mode = t, mode
	c.mu.Lock()
	c.receivers = append(c.receivers, r)
	c.mu.Unlock()
	return r, nil
}

func (c *fakeClient) AcceptSession(ctx context.Context, t target, sessionID string, mode azservicebus.ReceiveMode) (sbReceiver, error) {
	var (
		r   *fakeReceiver
		err error
	)
	if c.acceptFn != nil {
		r, err = c.acceptFn(ctx, t, sessionID, mode)
		if err != nil {
			return nil, err
		}
	} else {
		r = &fakeReceiver{sessionID: sessionID}
	}
	r.t, r.mode = t, mode
	c.mu.Lock()
	c.sessions = append(c.sessions, r)
	c.mu.Unlock()
	return r, nil
}

func (c *fakeClient) Close(context.Context) error { c.closed.Store(true); return nil }

func (c *fakeClient) receiverCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.receivers)
}

// --- fake admin -------------------------------------------------------------

type fakeAdmin struct {
	info    *entityInfo
	err     error
	calls   int
	forSend []bool
}

func (a *fakeAdmin) EntityInfo(_ context.Context, _ target, forSend bool) (*entityInfo, error) {
	a.calls++
	a.forSend = append(a.forSend, forSend)
	return a.info, a.err
}

// withFakes installs a fake client / admin and restores the production
// factories on cleanup.
func withFakes(t *testing.T) (*fakeClient, *fakeAdmin) {
	t.Helper()
	fc := newFakeClient()
	fa := &fakeAdmin{}
	prevC, prevA, prevB := connectClient, connectAdmin, listenerBackoff
	connectClient = func(*url.URL, target) (sbClient, error) { return fc, nil }
	connectAdmin = func(*url.URL, target) (sbAdmin, error) { return fa, nil }
	listenerBackoff = 5 * time.Millisecond
	t.Cleanup(func() { connectClient, connectAdmin, listenerBackoff = prevC, prevA, prevB })
	return fc, fa
}

// --- observer ---------------------------------------------------------------

type recordingObserver struct {
	mu       sync.Mutex
	sends    []error
	receives []error
	acks     int
	nacks    []bool
}

func (o *recordingObserver) OnSend(_ *url.URL, _ messaging.Message, err error, _ time.Duration) {
	o.mu.Lock()
	o.sends = append(o.sends, err)
	o.mu.Unlock()
}

func (o *recordingObserver) OnReceive(_ *url.URL, _ messaging.Message, err error) {
	o.mu.Lock()
	o.receives = append(o.receives, err)
	o.mu.Unlock()
}

func (o *recordingObserver) OnAck(*url.URL, messaging.Message) {
	o.mu.Lock()
	o.acks++
	o.mu.Unlock()
}

func (o *recordingObserver) OnNack(_ *url.URL, _ messaging.Message, requeue bool) {
	o.mu.Lock()
	o.nacks = append(o.nacks, requeue)
	o.mu.Unlock()
}

func (o *recordingObserver) snapshot() (sends, receives []error, acks int, nacks []bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]error(nil), o.sends...), append([]error(nil), o.receives...), o.acks, append([]bool(nil), o.nacks...)
}

// --- helpers ----------------------------------------------------------------

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return u
}

func newMsg(t *testing.T, p *Provider, body string) *MessageServiceBus {
	t.Helper()
	m, err := p.NewMessage(ServiceBusScheme)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if _, err := m.SetBodyStr(body); err != nil {
		t.Fatalf("SetBodyStr: %v", err)
	}
	return m.(*MessageServiceBus)
}

func strp(s string) *string { return &s }

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", msg)
}

func sbErr(code azservicebus.Code) error { return &azservicebus.Error{Code: code} }
