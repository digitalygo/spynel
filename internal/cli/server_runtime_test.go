package cli

import (
	"testing"

	"github.com/digitalygo/spynel/internal/app"
	"github.com/digitalygo/spynel/internal/core"
)

func TestPublishTUIStateChangesCarriesGlobalJobActivity(t *testing.T) {
	events := tuiStateEvents{runtime: make(chan core.RuntimeStatus, 1)}
	previous := app.SharedState{}
	for _, live := range []int{1, 2, 1, 0} {
		// Keep the registered count unchanged to catch running/settling edges.
		next := app.SharedState{Runtime: core.RuntimeStatus{Jobs: 2, LiveJobs: live}}
		publishTUIStateChanges(events, previous, next, "")
		select {
		case got := <-events.runtime:
			if got != next.Runtime {
				t.Fatalf("global activity update = %#v, want %#v", got, next.Runtime)
			}
		default:
			t.Fatalf("live job transition to %d was not published", live)
		}
		previous = next
	}
}
