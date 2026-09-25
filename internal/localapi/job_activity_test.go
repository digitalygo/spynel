package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitalygo/spynel/internal/app"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/instance"
)

func TestGlobalJobActivityReachesPrimaryAndAttachedTUI(t *testing.T) {
	root := t.TempDir()
	election, server, _, cancel, done := startTestServer(t, root)
	defer func() {
		cancel()
		<-done
		_ = server.Service.Close()
		lease, _ := election.Current()
		_ = election.Release(lease.Token)
	}()
	secondary, err := instance.New(server.Service.Settings.Snapshot().StatePath())
	if err != nil {
		t.Fatal(err)
	}
	clients := []*Client{NewClient(election), NewClient(secondary)}
	ctx := context.Background()
	check := func(registered, live int) {
		t.Helper()
		for _, client := range clients {
			// Registration is also the initial/reconnect refresh boundary.
			initial, err := client.RegisterLiveTUIState(ctx, "idle-displayed")
			if err != nil {
				t.Fatal(err)
			}
			state, err := client.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			status, err := client.Status(ctx, "idle-displayed")
			if err != nil {
				t.Fatal(err)
			}
			if initial.Runtime != state.Runtime || state.Runtime != status.Runtime || state.Runtime.Jobs != registered || state.Runtime.LiveJobs != live {
				t.Fatalf("registration/poll/status disagree: initial=%#v state=%#v status=%#v", initial.Runtime, state.Runtime, status.Runtime)
			}
			if status.TurnActive {
				t.Fatalf("global job manufactured local turn activity: %#v %#v", state, status)
			}
		}
	}
	runtime := server.Service.Runtime
	check(0, 0)
	first := runtime.BeginJob("chat:telegram:elsewhere", "telegram", "elsewhere", "remote conversation")
	runtime.SetJobRunningIfStarting(first)
	check(1, 1)
	second := runtime.BeginJob("chat:tui:other", "tui", "other", "another TUI")
	check(2, 2)
	runtime.UpdateJob(first, core.ExecutionStatus{State: string(app.JobReconnecting)})
	check(2, 2)
	runtime.EndJob(second)
	check(1, 1)
	runtime.UpdateJob(first, core.ExecutionStatus{State: string(app.JobRunning)})
	check(1, 1)
	runtime.UpdateJob(first, core.ExecutionStatus{State: string(app.JobFinishing)})
	check(1, 0)
	runtime.EndJob(first)
	check(0, 0)

	for _, relative := range []string{"tasks/todo/queued.md", "goals/active/passive.md"} {
		path := filepath.Join(root, ".spynel", relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nid: pending\n---\n# Pending work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := clients[1].State(ctx)
	status, statusErr := clients[1].Status(ctx, "idle-displayed")
	if err != nil || statusErr != nil || state.Runtime.Jobs != 0 || state.Runtime.LiveJobs != 0 || status.TurnActive {
		t.Fatalf("legacy task/goal files manufactured activity: state=%#v status=%#v state_error=%v status_error=%v", state, status, err, statusErr)
	}
	assertRetiredWorkflowFieldsAbsent(t, clients[1], "/v1/state")
	assertRetiredWorkflowFieldsAbsent(t, clients[1], "/v1/status?conversation=idle-displayed")
}

// assertRetiredWorkflowFieldsAbsent fetches one authenticated state or status
// payload through the ordinary client transport and verifies the retired
// task/goal workflow fields never reappear on the wire.
func assertRetiredWorkflowFieldsAbsent(t *testing.T, client *Client, path string) {
	t.Helper()
	response, err := client.request(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("GET %s decode: %v", path, err)
	}
	for _, field := range []string{
		"durable_work", "work_count_diagnostics",
		"orchestrator_leases", "orchestrator_dispatches",
		"tasks_active", "tasks_waiting", "goals_active",
		"heartbeat_state", "next_heartbeat_at", "scheduled_goal_checkpoints",
		"conversation_recovery",
	} {
		if _, ok := payload[field]; ok {
			t.Fatalf("GET %s still exposes retired workflow field %q", path, field)
		}
	}
}
