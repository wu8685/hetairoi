package eventbus

import (
	"context"
	"log"
	"time"
)

// Source is an event-ingestion source. It runs until ctx is cancelled, pushing
// events into the bus it was built against. WebhookHandler is the push
// counterpart (the caller delivers events); Source is the pull counterpart (the
// source fetches them). Built-in sources let cma-service ingest events from
// upstreams that cannot call our webhook (e.g. an CodeHub project polled over
// its CLI).
type Source interface {
	Name() string
	Run(ctx context.Context) error
}

// FetchFunc returns the current batch of events for one poll tick. Returning the
// same Event.ID across ticks is expected and harmless: the bus dedups by ID, so
// an unchanged upstream item drives no work and does not pile up. Encode the
// item's mutable version (e.g. a head commit sha) into Event.ID so that a real
// change yields a new ID — and therefore exactly one new turn.
type FetchFunc func(ctx context.Context) ([]Event, error)

// Poller is the generic interval Source: every interval it calls fetch and
// dispatches each returned event through the bus. Suppression of repeated
// identical items is the bus's job (Event.ID dedup), not the poller's.
type Poller struct {
	source   string
	interval time.Duration
	fetch    FetchFunc
	bus      *Bus
	onResult func([]DispatchResult)
}

// NewPoller builds an interval Source named name that drives bus from fetch.
// A non-positive interval defaults to 30s.
func NewPoller(name string, interval time.Duration, bus *Bus, fetch FetchFunc) *Poller {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Poller{source: name, interval: interval, fetch: fetch, bus: bus}
}

// OnResult registers a callback invoked with the dispatch results of every
// emitted event (for logging/observability). Returns p for chaining.
func (p *Poller) OnResult(fn func([]DispatchResult)) *Poller {
	p.onResult = fn
	return p
}

func (p *Poller) Name() string { return p.source }

// Run polls until ctx is cancelled. It fires once immediately, then every
// interval. A fetch error is logged and the loop continues — a transient
// upstream failure must not kill the source.
func (p *Poller) Run(ctx context.Context) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	p.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	evs, err := p.fetch(ctx)
	if err != nil {
		log.Printf("eventbus: source %q fetch: %v", p.source, err)
		return
	}
	for _, e := range evs {
		res := p.bus.Dispatch(e)
		if p.onResult != nil {
			p.onResult(res)
		}
	}
}
