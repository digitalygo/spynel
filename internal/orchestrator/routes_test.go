package orchestrator

import (
	"testing"
	"time"
)

func TestWorkflowRouteStaleThresholds(t *testing.T) {
	for _, test := range []struct {
		name string
		want time.Duration
	}{
		{name: "tasks", want: 4 * time.Hour},
		{name: "goals", want: 12 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			route, ok := routeByName(test.name)
			if !ok {
				t.Fatalf("workflow route %q is missing", test.name)
			}
			if route.StaleAfter != test.want {
				t.Fatalf("route %q StaleAfter = %s, want %s", route.Name, route.StaleAfter, test.want)
			}
		})
	}
}
