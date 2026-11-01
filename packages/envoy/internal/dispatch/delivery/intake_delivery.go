package delivery

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// intakeDelivery is one subscription of an intake's: the client it subscribed on, the
// subscription, the keyed runner its callback (receive) hands messages to, the hold it delivers
// under, and the context every handler of it runs under. Each subscribe makes one of its own, so
// stopping one ends that subscription and nothing a later subscribe starts.
type intakeDelivery struct {
	nats *bus.Client
	sub  *natsgo.Subscription
	keys *keyedRunner[intakeMessage]
	// key reads whether this delivery wants a message at all and the key its handler runs under
	// (heldDurable.key).
	key func(*natsgo.Msg) (string, *contracts.Envelope, bool)
	// hold is the hold the subscription delivers under: once its loss deadline has passed, every
	// message is handed back rather than handled, since this intake may no longer act on the
	// durable's messages.
	hold *duty.Hold
	// handling is the parent of every handler's context (deliver), and abandon cancels it: the stop
	// gives up the handlers still running when its context ends.
	handling context.Context
	abandon  context.CancelFunc
	stopping atomic.Bool
	stopped  sync.Once

	mu sync.Mutex
	// held is what the stop hands back to NATS once no handler of this delivery is running: the
	// messages that were waiting behind their keys, and those the callback was handed since.
	held []*natsgo.Msg
	// released is set once held has been handed back; the callback then hands a message back at
	// once.
	released bool
}

// intakeMessage is one message on its way to its key's handler, with its envelope when keying it
// decoded the envelope (a workflow run's), so the handler does not decode it again.
type intakeMessage struct {
	msg      *natsgo.Msg
	envelope *contracts.Envelope
}

// subscribe subscribes the intake's handling to its durable's push deliveries at deliverSubject
// (subscribeDurable), for a caller that creates the durable with it right after (takeOver). The
// subscription's callback only routes each message (receive); its handler runs on the goroutine of
// the message's key (keyedRunner) and acknowledges it there. StopDelivering stops the subscription
// this made last, so a caller subscribing again stops the one before first. A subscribe that fails
// gives up its handling and the registration the client kept of it (bus.Client.ReleaseFailed): a
// reconnect would bind that registration otherwise, and its callback, its handling given up, would
// hand back every message until the intake's next subscribe replaced it.
func (d *heldDurable) subscribe(deliverSubject string) (*natsgo.Subscription, error) {
	handling, abandon := context.WithCancel(context.Background())
	delivery := &intakeDelivery{nats: d.nats, key: d.key, hold: d.hold, handling: handling, abandon: abandon}
	delivery.keys = newKeyedRunner(func(message intakeMessage) { d.handle(handling, message) })
	wanted := d.config()
	sub, err := subscribeDurable(d.nats, wanted, deliverSubject, delivery.receive)
	if err != nil {
		abandon()
		d.nats.ReleaseFailed(wanted.FilterSubject)
		return nil, err
	}
	delivery.sub = sub
	d.mu.Lock()
	d.delivering = delivery
	d.mu.Unlock()
	return sub, nil
}

// subscribeDurable subscribes deliver to wanted's durable through client.Subscribe at
// deliverSubject, acknowledging by hand. Bind with DeliverSubject and SkipConsumerLookup
// subscribes without looking the durable up, so the durable need not exist yet: nats.go sends
// the SUB and creates nothing (js.go, the SkipConsumerLookup branch of subscribe with Bind). It
// is for a caller that creates the durable with that deliver subject right after, under its
// consumer lease (takeOver).
func subscribeDurable(client *bus.Client, wanted natsgo.ConsumerConfig, deliverSubject string, deliver natsgo.MsgHandler) (*natsgo.Subscription, error) {
	options := []natsgo.SubOpt{
		natsgo.Bind(bus.Stream, wanted.Durable),
		natsgo.ManualAck(),
		natsgo.DeliverSubject(deliverSubject),
		natsgo.SkipConsumerLookup(),
	}
	// The subject must be the consumer's own filter subject: nats.go checks a bound
	// subscription's subject against it, and refuses an empty one ("subject does not match
	// consumer").
	return client.Subscribe(wanted.FilterSubject, deliver, options...)
}

// intakeStopBudget is the one deadline of an intake's stop, counted from the moment Run's ctx ends
// (stopContext): the messages in their handlers have until then to finish and be acknowledged
// before the stop gives them up (StopDelivering), the drain's wait ends by then, and a take-over
// under way as ctx ended ends by then too (takeOverFinishWithin, 10 s), so one that binds after
// ctx ended is stopped under the same deadline rather than one of its own. In cmd/dispatch that
// ctx is the signal context, so the stop begins at SIGTERM and runs beside the HTTP drain and the
// document shutdown, which take up to stopGrace (30 s) less a 5 s reserve: httpDrainBeforeDocuments
// then documentShutdownTimeout (cmd/dispatch/shutdown.go); StartIntakes' stop, which the shutdown
// waits for, adds no deadline of its own. The shutdown waits for the intakes after the document
// service and before it deregisters this process and closes the database pool, so that a handler's
// writes never meet a closed pool. 15 s leaves 10 s before the document budget's latest end for
// what follows the deadline -- the handlers given up returning, which they do as soon as their
// context is cancelled, the negative acknowledgements, the drain's end, which the given-up
// handlers' cancelled context cuts short, the hold's release and the clients closing -- so the
// intakes never hold the shutdown past what that budget already can, and 15 s before the kubelet's
// kill. It is also well past what a handler GitHub answers needs, a pull request and its commit
// pages with a page GitHub answered 502 asked again (after 1 s, then 2 s: githubPageRetryWait), so
// an ordinary handler finishes and is acknowledged rather than redone. A call GitHub does not
// answer holds one attempt for up to githubapp.RequestTimeout (20 s), and is then asked again,
// which no budget inside stopGrace could wait out. A var so a test can shrink it.
var intakeStopBudget = 15 * time.Second

// stopContext returns the stop deadline of an intake whose Run runs until ctx ends: a context that
// ends intakeStopBudget after ctx does, armed by ctx's end, so everything of the stop -- a
// take-over under way at that moment, the handler wait, the drain's wait -- runs under one
// deadline counted from it. release frees it, for run to defer.
func stopContext(ctx context.Context) (stopping context.Context, release func()) {
	// Read now rather than as ctx ends, so a test restoring intakeStopBudget after Run has
	// returned never races the read.
	budget := intakeStopBudget
	stopping, cancel := context.WithCancel(context.WithoutCancel(ctx))
	disarm := context.AfterFunc(ctx, func() { time.AfterFunc(budget, cancel) })
	return stopping, func() {
		disarm()
		cancel()
	}
}

// StopDelivering stops this intake's subscription so that another process can take the durable
// over with nothing waiting out the ack wait. It drains the subscription, so the server delivers it
// nothing more, and waits for the messages in their handlers until ctx ends: one that finishes by
// then is acknowledged, and the handlers still running when ctx ends are given up -- their context
// is cancelled, so a GitHub call that does not answer is abandoned -- and their messages
// negatively acknowledged as they return. Only once no handler is running does it negatively
// acknowledge every message that was waiting behind its key and every message the callback was
// handed meanwhile, those NATS had already delivered to the client's buffer included, so the server
// redelivers them at once -- to the next holder, which can then not start a pull request's or run's
// next message while this process still works on the one before it. A message the callback is
// handed after that is handed back at once. It returns once the drain has ended -- at the latest
// intakeDrainWait after the last handler returned, or as ctx ends if that is sooner, when it
// unsubscribes instead. The drain gives up the subscription's registration on the client
// (bus.Client.DrainSubscription), so that no reconnect binds it again and a later subscribe on the
// same client starts afresh.
//
// run calls it as its ctx ends, under its stop deadline (stopContext), and when the hold is lost
// or the durable no longer delivers here, under a budget of its own (stopDeliveringWithin). It is
// safe from any goroutine and idempotent: a second call returns once the first has stopped, and a
// call with nothing subscribed returns at once. Every call's ctx bounds the handler wait of the
// stop under way, so a caller with a nearer deadline than the first has the handlers given up when
// its own ctx ends.
func (d *heldDurable) StopDelivering(ctx context.Context) {
	d.mu.Lock()
	delivery := d.delivering
	d.mu.Unlock()
	if delivery != nil {
		delivery.stop(ctx)
	}
}

// stop is StopDelivering for this subscription. The drain goes first, so the server has the
// subscription's interest gone before any negative acknowledgement reaches it, and redelivers
// what this one gives back to the next subscriber rather than straight back here. It drains the
// subscription the client holds for this one's registration (bus.Client.DrainSubscription): a
// handle of the client's own once it has replaced its connection, not sub, which went with the
// replaced connection while the client's handle went on delivering here. The client holds nothing
// of this stopped callback's from the drain on.
func (d *intakeDelivery) stop(ctx context.Context) {
	giveUp := context.AfterFunc(ctx, d.abandon)
	defer giveUp()
	d.stopped.Do(func() {
		d.stopping.Store(true)
		draining, err := d.nats.DrainSubscription(d.sub)
		if err != nil {
			slog.Error("dispatch delivery: drain the intake's subscription", "error", err)
		}
		for _, waiting := range d.keys.stop() {
			d.giveBack(waiting.msg)
		}
		// Returns once every handler has, which a handler given up (abandon) does promptly: its
		// GitHub calls and its writes all run under its context.
		d.keys.wait()
		d.mu.Lock()
		d.released = true
		held := d.held
		d.held = nil
		d.mu.Unlock()
		for _, msg := range held {
			negativelyAcknowledge(msg)
		}
		// nats.go ends a drain on a goroutine of its own, once the callback has run every message
		// the client had buffered, and reports it only by the subscription turning invalid.
		waitedFrom := time.Now()
		for draining != nil && draining.IsValid() {
			if d.handling.Err() != nil || time.Since(waitedFrom) >= intakeDrainWait {
				slog.Error("dispatch delivery: the intake's subscription did not finish draining; unsubscribing it", "waited", time.Since(waitedFrom))
				if err := draining.Unsubscribe(); err != nil {
					slog.Error("dispatch delivery: unsubscribe the intake's subscription", "error", err)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		d.abandon()
	})
}

// giveBack hands msg back to NATS for the next holder: once no handler of this delivery is
// running, at once; until then it is held, and the stop hands it back with the rest. Handed back
// earlier, a message of a key whose handler is still running here could be redelivered to the
// next holder and run there beside it, and two fetches of one pull request or run at once can
// write the older answer last.
func (d *intakeDelivery) giveBack(msg *natsgo.Msg) {
	d.mu.Lock()
	if !d.released {
		d.held = append(d.held, msg)
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	negativelyAcknowledge(msg)
}

// intakeDrainWait bounds how long stop waits for the drain to end once every handler has returned.
// By then all that is left is nats.go's: the callback handing back what the client had buffered,
// which never blocks, and the drain's flush to the server. So the bound is the one nats.go itself
// gives a connection's drain, natsgo.DefaultDrainTimeout -- bus connections never set their own --
// rather than intakeMessageTimeout, which bounds a handler, and the handlers are done. The stop's
// context ending cuts it shorter: by then the drain has had the whole handler wait to end.
const intakeDrainWait = natsgo.DefaultDrainTimeout

// receive is the subscription's callback, and only routes: a message once stopping is set, or once
// the hold's loss deadline has passed, is handed back (giveBack) -- this intake may no longer act
// on the durable's messages, and the server redelivers them to the next holder; a subject the
// intake's key does not want is acknowledged; and any other message goes to its key's handler
// (deliver).
func (d *intakeDelivery) receive(msg *natsgo.Msg) {
	if d.stopping.Load() || (d.hold != nil && !time.Now().Before(d.hold.Deadline())) {
		d.giveBack(msg)
		return
	}
	key, envelope, wanted := d.key(msg)
	if !wanted {
		acknowledge(msg)
		return
	}
	if !d.keys.run(key, intakeMessage{msg: msg, envelope: envelope}) {
		d.giveBack(msg)
	}
}

// intakeKey reads from a message's subject, notifications.github.<owner>.<repo>.<kind>..., whether
// the intake wants it, and the key its handler runs under. Only two shapes can carry a kind route
// handles: a pull request's own subject, pr.<n> and nothing after it (pr.<n>.review and
// pr.<n>.comment carry other kinds, contracts' githubTopic), and a workflow run's,
// workflow.<file>.<action>. Every other subject -- checks, comments, issues, pushes, job events --
// is not wanted, and is decided with no decode at all.
//
// A pull request is keyed by its subject's repository and number, with no decode. A workflow
// run's subject names its file and not its run, so its key is its payload's repo and run_id, and
// the envelope decoded for them is handed on. A workflow message whose envelope or payload does
// not decode is keyed by its subject, and its handler logs it.
func intakeKey(msg *natsgo.Msg) (key string, envelope *contracts.Envelope, wanted bool) {
	tokens := strings.Split(msg.Subject, ".")
	if len(tokens) < 5 || tokens[0] != "notifications" || tokens[1] != "github" {
		return "", nil, false
	}
	switch {
	case tokens[4] == "pr" && len(tokens) == 6:
		return "pr " + tokens[2] + "/" + tokens[3] + "#" + tokens[5], nil, true
	case tokens[4] == "workflow" && len(tokens) == 7:
		decoded, err := decodeEnvelope(msg.Data)
		if err != nil {
			return "workflow " + msg.Subject, nil, true
		}
		var run struct {
			Repo  string `json:"repo"`
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal([]byte(decoded.Payload), &run); err != nil {
			return "workflow " + msg.Subject, &decoded, true
		}
		return "workflow " + run.Repo + "#" + run.RunID, &decoded, true
	}
	return "", nil, false
}

// decodeEnvelope decodes one message's envelope. A var so a test can count the decodes.
var decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
	var envelope contracts.Envelope
	err := json.Unmarshal(data, &envelope)
	return envelope, err
}
