package trace

import "sync"

// maxPendingDeltaBytes bounds the text queued for one subscriber that has
// stopped reading. Past it the queued text is dropped and the subscriber is
// told to discard what it has shown.
const maxPendingDeltaBytes = 1 << 20

// ItemKind names what a subscriber should do with an Item.
type ItemKind int

const (
	// ItemPersisted says the store holds events the subscriber has not read.
	ItemPersisted ItemKind = iota
	// ItemDelta carries model text. It has no cursor and is never stored.
	ItemDelta
	// ItemReset says text already sent will not appear in the stored events:
	// the response failed, or queued text was dropped. Discard what was shown
	// for the current model response.
	ItemReset
	// ItemEnd marks the end of a model response. Text queued ahead of it
	// belongs to a response the store may already report as complete.
	ItemEnd
)

// Item is one entry in a subscriber's queue.
type Item struct {
	Kind ItemKind
	Text string
}

// Hub fans one run's activity out to its live subscribers in arrival order.
// It stores no events: a subscriber reads durable events from the store. The
// only text it keeps is that of a model response still being streamed, so a
// subscriber that joins midway starts from the beginning of that response.
type Hub struct {
	mu      sync.Mutex
	subs    map[string]map[*Subscription]struct{}
	current map[string]*partial
}

// partial is the text streamed so far for a response that has not ended.
type partial struct {
	text      []byte
	truncated bool
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{} }

// Subscription is one listener's ordered queue.
type Subscription struct {
	hub   *Hub
	runID string
	wake  chan struct{}

	mu      sync.Mutex
	items   []Item
	pending int
}

// Subscribe starts queueing activity for runID. If a model response is being
// streamed, the queue starts with its text so far. Call Close when done.
func (h *Hub) Subscribe(runID string) *Subscription {
	sub := &Subscription{hub: h, runID: runID, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = make(map[string]map[*Subscription]struct{})
	}
	if h.subs[runID] == nil {
		h.subs[runID] = make(map[*Subscription]struct{})
	}
	h.subs[runID][sub] = struct{}{}
	if p := h.current[runID]; p != nil && !p.truncated && len(p.text) > 0 {
		sub.push(Item{Kind: ItemDelta, Text: string(p.text)})
	}
	return sub
}

// Close stops delivery. It is safe to call more than once.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[s.runID]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(h.subs, s.runID)
		}
	}
}

// Wake receives when Take has something to return.
func (s *Subscription) Wake() <-chan struct{} { return s.wake }

// Take returns the queued items in arrival order and empties the queue.
func (s *Subscription) Take() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.items
	s.items = nil
	s.pending = 0
	return out
}

// Replayed tells the subscription that its owner has just read the stored
// events. A response that ended between Subscribe and that read may already
// be in what was read, so its queued text is dropped: the subscriber never
// sees text arrive after the event that completed it.
func (s *Subscription) Replayed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = afterLastEnd(s.items)
	s.pending = 0
	for _, item := range s.items {
		s.pending += len(item.Text)
	}
}

// afterLastEnd drops the text and resets queued ahead of the last ItemEnd.
func afterLastEnd(items []Item) []Item {
	last := -1
	for i, item := range items {
		if item.Kind == ItemEnd {
			last = i
		}
	}
	if last < 0 {
		return items
	}
	kept := make([]Item, 0, len(items))
	for i, item := range items {
		if i < last && item.Kind != ItemPersisted {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

func (s *Subscription) push(it Item) {
	s.mu.Lock()
	last := len(s.items) - 1
	switch {
	case it.Kind == ItemPersisted && last >= 0 && s.items[last].Kind == ItemPersisted:
		// One store read covers both.
	case it.Kind == ItemDelta && s.pending+len(it.Text) > maxPendingDeltaBytes:
		kept := s.items[:0]
		for _, queued := range s.items {
			if queued.Kind == ItemPersisted || queued.Kind == ItemEnd {
				kept = append(kept, queued)
			}
		}
		s.items = append(kept, Item{Kind: ItemReset}, it)
		s.pending = len(it.Text)
	case it.Kind == ItemDelta && last >= 0 && s.items[last].Kind == ItemDelta:
		s.items[last].Text += it.Text
		s.pending += len(it.Text)
	default:
		s.items = append(s.items, it)
		s.pending += len(it.Text)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) publish(runID string, it Item) {
	for sub := range h.subs[runID] {
		sub.push(it)
	}
}

// Notify tells runID's subscribers that the store has new events for it.
func (h *Hub) Notify(runID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publish(runID, Item{Kind: ItemPersisted})
}

// Delta sends model text to runID's subscribers. Empty text is ignored.
func (h *Hub) Delta(runID, text string) {
	if h == nil || text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		h.current = make(map[string]*partial)
	}
	p := h.current[runID]
	if p == nil {
		p = &partial{}
		h.current[runID] = p
	}
	switch {
	case p.truncated:
	case len(p.text)+len(text) > maxPendingDeltaBytes:
		// Too long to hand to a late subscriber; it waits for the next response.
		p.text, p.truncated = nil, true
	default:
		p.text = append(p.text, text...)
	}
	h.publish(runID, Item{Kind: ItemDelta, Text: text})
}

// End marks the end of runID's current model response. Its text is no longer
// handed to new subscribers. When the response did not complete, subscribers
// that were sent some of it are told to discard it.
func (h *Hub) End(runID string, completed bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.current[runID]
	delete(h.current, runID)
	if p == nil {
		return
	}
	if !completed {
		h.publish(runID, Item{Kind: ItemReset})
	}
	h.publish(runID, Item{Kind: ItemEnd})
}

// Subscribers reports how many listeners runID has.
func (h *Hub) Subscribers(runID string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[runID])
}
