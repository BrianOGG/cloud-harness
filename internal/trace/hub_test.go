package trace

import (
	"reflect"
	"strings"
	"testing"
)

func take(t *testing.T, sub *Subscription) []Item {
	t.Helper()
	select {
	case <-sub.Wake():
	default:
		t.Fatal("subscription was not woken")
	}
	return sub.Take()
}

func TestHubDeliversDeltasAndCommitsInArrivalOrder(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("run")
	defer sub.Close()
	other := h.Subscribe("other")
	defer other.Close()

	h.Notify("run")
	h.Notify("run")
	h.Delta("run", "a")
	h.Delta("run", "")
	h.Delta("run", "b")
	h.Notify("run")
	h.Delta("run", "c")

	want := []Item{{Kind: ItemPersisted}, {Kind: ItemDelta, Text: "ab"}, {Kind: ItemPersisted}, {Kind: ItemDelta, Text: "c"}}
	if got := take(t, sub); !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}
	if got := other.Take(); len(got) != 0 {
		t.Fatalf("another run's subscriber received %+v", got)
	}
}

func TestHubLateSubscriberStartsFromTheResponseSoFar(t *testing.T) {
	h := NewHub()
	h.Delta("run", "Hello, ")
	h.Delta("run", "wor")
	sub := h.Subscribe("run")
	defer sub.Close()
	h.Delta("run", "ld")

	want := []Item{{Kind: ItemDelta, Text: "Hello, world"}}
	if got := take(t, sub); !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}

	// Once the response has ended, a new subscriber is not handed its text.
	h.End("run", true)
	late := h.Subscribe("run")
	defer late.Close()
	if got := late.Take(); len(got) != 0 {
		t.Fatalf("a subscriber that joined after the response received %+v", got)
	}
}

// A subscriber reads the stored events after Subscribe. A response that ended
// before that read may already be in it, so its text must not follow.
func TestHubReplayedOmitsTextOfAnEndedResponse(t *testing.T) {
	h := NewHub()
	h.Delta("run", "first ")
	sub := h.Subscribe("run")
	defer sub.Close()
	h.Delta("run", "response")
	h.End("run", true)
	h.Notify("run")
	h.Delta("run", "second")
	sub.Replayed()

	want := []Item{{Kind: ItemEnd}, {Kind: ItemPersisted}, {Kind: ItemDelta, Text: "second"}}
	if got := take(t, sub); !reflect.DeepEqual(got, want) {
		t.Fatalf("after replay = %+v, want %+v", got, want)
	}

	// A response that ends after the read keeps its text.
	h.Delta("run", " response")
	h.End("run", true)
	h.Notify("run")
	want = []Item{{Kind: ItemDelta, Text: " response"}, {Kind: ItemEnd}, {Kind: ItemPersisted}}
	if got := take(t, sub); !reflect.DeepEqual(got, want) {
		t.Fatalf("later take = %+v, want %+v", got, want)
	}
}

func TestHubFailedResponseResetsWhatWasShown(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("run")
	defer sub.Close()
	if got := sub.Take(); len(got) != 0 {
		t.Fatalf("items = %+v", got)
	}
	h.Delta("run", "partial")
	h.End("run", false)
	want := []Item{{Kind: ItemDelta, Text: "partial"}, {Kind: ItemReset}, {Kind: ItemEnd}}
	if got := take(t, sub); !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}

	// A response that streamed no text has nothing to reset.
	h.End("run", false)
	if got := sub.Take(); len(got) != 0 {
		t.Fatalf("items = %+v", got)
	}
}

func TestHubBoundsTextQueuedForASlowSubscriber(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("run")
	defer sub.Close()
	sub.Take()
	chunk := strings.Repeat("x", maxPendingDeltaBytes/2+1)
	h.Notify("run")
	h.Delta("run", chunk)
	h.Delta("run", chunk)

	want := []Item{{Kind: ItemPersisted}, {Kind: ItemReset}, {Kind: ItemDelta, Text: chunk}}
	if got := take(t, sub); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %d items, kinds %v", len(got), kinds(got))
	}

	// The response is now too long to hand to a subscriber that joins midway.
	late := h.Subscribe("run")
	defer late.Close()
	if got := late.Take(); len(got) != 0 {
		t.Fatalf("late subscriber received kinds %v", kinds(got))
	}
}

func TestHubCloseStopsDelivery(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("run")
	if n := h.Subscribers("run"); n != 1 {
		t.Fatalf("subscribers = %d", n)
	}
	sub.Close()
	sub.Close()
	if n := h.Subscribers("run"); n != 0 {
		t.Fatalf("subscribers after close = %d", n)
	}
	h.Delta("run", "ignored")
	if got := sub.Take(); len(got) != 0 {
		t.Fatalf("closed subscription received %+v", got)
	}
}

func kinds(items []Item) []ItemKind {
	out := make([]ItemKind, len(items))
	for i, item := range items {
		out[i] = item.Kind
	}
	return out
}
