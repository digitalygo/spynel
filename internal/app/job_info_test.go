package app

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/workspace"
)

func newJobInfoService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, newServiceHarness())
}

func runJobCommand(t *testing.T, service *Service, command string) string {
	t.Helper()
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) {
		if event.Kind == core.EventFinal {
			response = event
		}
	}); err != nil {
		t.Fatal(err)
	}
	return response.Text
}

func TestJobInfoShowsConversationJobMetadata(t *testing.T) {
	service := newJobInfoService(t)
	service.Runtime.BeginJob("chat:tui:local", "tui", "local", "answer a question")
	output := runJobCommand(t, service, "/job info 1")
	for _, want := range []string{"# Job 1", "Kind: conversation", "Route: tui", "Provider steps (▶): 1 (live conversation)", "Current execution age:", "Started:", "Health:"} {
		if !strings.Contains(output, want) {
			t.Fatalf("job info missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"Durable work", "Lease:", "Recovery count", "Implementation attempts", "Durable ID", "Recent progress"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("retired workflow metadata leaked into job info (%q):\n%s", forbidden, output)
		}
	}
}

func TestDurableJobsListGloballyAcrossInterfacesWithoutNotificationOrigin(t *testing.T) {
	service := newJobInfoService(t)
	service.Runtime.BeginJob("chat:cli:automation", "cli", "automation", "run a task")
	var expected string
	for _, caller := range []core.Message{
		{Channel: "tui", Conversation: "local", Text: "/jobs"},
		{Channel: "telegram", Conversation: "TG-other", Text: "/jobs"},
		{Channel: "whatsapp", Conversation: "WA-other", Text: "/jobs"},
		{Channel: "cli", Conversation: "automation", Text: "/jobs"},
	} {
		var response core.Event
		if err := service.Handle(context.Background(), caller, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if expected == "" {
			expected = response.Text
		} else if response.Text != expected {
			t.Fatalf("%s jobs output differs:\n%s\n--- want ---\n%s", caller.Channel, response.Text, expected)
		}
	}
	if !strings.Contains(expected, "Job 1") {
		t.Fatalf("global jobs projection is incomplete:\n%s", expected)
	}
}

func TestJobInfoAndOutputCommandsRejectInvalidInput(t *testing.T) {
	service := newJobInfoService(t)
	for _, test := range []struct {
		command string
		want    string
	}{
		{"/job info", "Usage:"},
		{"/job info nope", "from 1 to 9999"},
		{"/job info 99", "was not found"},
		{"/job inspect 1", "Usage:"},
		{"/job message 1 hello", "Usage:"},
		{"/job ping 1", "Usage:"},
		{"/job output", "Usage:"},
	} {
		if output := runJobCommand(t, service, test.command); !strings.Contains(output, test.want) || !strings.Contains(output, "/jobs") {
			t.Fatalf("%s = %q, want %q", test.command, output, test.want)
		}
	}
}

func TestArchivedJobCommandsRejectMismatchedEmbeddedIdentityAndCleanupRemovesOldRecord(t *testing.T) {
	service := newJobInfoService(t)
	service.Runtime.Now = func() time.Time {
		return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	}
	id := service.Runtime.BeginJob("identity", "cli", "private", "conversation")
	service.Runtime.RecordJobEvent(id, core.Event{Kind: core.EventFinal, Text: "must not resolve under another identity", Done: true})
	job, ok := service.Runtime.Job(id)
	if !ok {
		t.Fatal("job was not started")
	}
	service.Runtime.EndJob(id)

	path := service.Config.StatePath("jobs", job.StableID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedID := "j-20260801T120000Z-deadbeef"
	if mismatchedID == job.StableID {
		mismatchedID = "j-20260801T120000Z-feedface"
	}
	corrupt := strings.Replace(string(data), "id: \""+job.StableID+"\"", "id: \""+mismatchedID+"\"", 1)
	if corrupt == string(data) {
		t.Fatal("archive identity was not replaced")
	}
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	if output := runJobCommand(t, service, "/jobs recent"); !strings.Contains(output, "No archived jobs are available") || strings.Contains(output, mismatchedID) {
		t.Fatalf("recent jobs exposed mismatched archive:\n%s", output)
	}
	for _, command := range []string{"/job info " + strconv.Itoa(job.Number), "/job output " + strconv.Itoa(job.Number)} {
		if output := runJobCommand(t, service, command); !strings.Contains(output, "not found") || strings.Contains(output, mismatchedID) {
			t.Fatalf("%s resolved mismatched archive:\n%s", command, output)
		}
	}

	old := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, removedBytes, protected, failed := service.Runtime.CleanupArchivedJobs(old.Add(8 * 24 * time.Hour))
	if removed != 1 || removedBytes <= 0 || protected != 0 || failed != 0 {
		t.Fatalf("cleanup = removed %d bytes %d protected %d failed %d", removed, removedBytes, protected, failed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mismatched archive remains after cleanup: %v", err)
	}
}

func TestArchivedJobCommandsPreserveLiveStateAndMarkRestartInterruption(t *testing.T) {
	service := newJobInfoService(t)
	id := service.Runtime.BeginJobWithDetails("chat:cli:archive-state", "cli", "private", "conversation", JobDetails{Kind: "conversation"})
	service.Runtime.UpdateJob(id, core.ExecutionStatus{State: "reconnecting", ReconnectAttempt: 2, ReconnectTotal: 5})
	service.Runtime.RecordJobEvent(id, core.Event{Kind: core.EventStatus, Text: "provider reconnect pending"})
	job, ok := service.Runtime.Job(id)
	if !ok {
		t.Fatal("live job disappeared")
	}

	for _, command := range []string{"/job info 1", "/job output 1"} {
		output := runJobCommand(t, service, command)
		if !strings.Contains(output, "reconnecting") || strings.Contains(output, "interrupted") {
			t.Fatalf("%s discarded exact live state:\n%s", command, output)
		}
	}

	restarted := New(service.Config, newServiceHarness())
	output := runJobCommand(t, restarted, "/job info "+strconv.Itoa(job.Number))
	if !strings.Contains(output, "State: interrupted") || strings.Contains(output, "reconnecting") {
		t.Fatalf("restart did not classify the unfinished archive safely:\n%s", output)
	}
}

func TestCompletedJobKillReportsLiveOnlySemantics(t *testing.T) {
	service := newJobInfoService(t)
	handle := service.Runtime.BeginJob("completed-kill", "cli", "private", "conversation")
	job, _ := service.Runtime.Job(handle)
	service.Runtime.EndJob(handle)
	output := runJobCommand(t, service, "/job kill "+strconv.Itoa(job.Number))
	if !strings.Contains(output, "completed and cannot be killed") || !strings.Contains(output, "only for live jobs") {
		t.Fatalf("completed kill response = %q", output)
	}
}

func TestJobInfoIsInSharedCommandCatalog(t *testing.T) {
	foundInfo := false
	foundKill := false
	foundMessage := false
	foundPing := false
	foundOutput := false
	foundRecent := false
	jobsIndex := -1
	recentIndex := -1
	for index, command := range SlashCommands() {
		if command.Usage == "/jobs" {
			jobsIndex = index
		}
		if command.Usage == "/jobs recent" {
			recentIndex = index
		}
		foundInfo = foundInfo || command.Usage == "/job info <number>"
		foundKill = foundKill || command.Usage == "/job kill <number>"
		foundMessage = foundMessage || command.Usage == "/job message <number> <text>"
		foundPing = foundPing || command.Usage == "/job ping <number>"
		foundOutput = foundOutput || command.Usage == "/job output <number> [tail <bytes>]"
		foundRecent = foundRecent || command.Usage == "/jobs recent"
	}
	if !foundInfo || !foundKill || !foundOutput || !foundRecent {
		t.Fatalf("job commands in catalog: recent=%t info=%t output=%t kill=%t", foundRecent, foundInfo, foundOutput, foundKill)
	}
	if foundMessage || foundPing {
		t.Fatalf("retired job control commands remain in catalog: message=%t ping=%t", foundMessage, foundPing)
	}
	if jobsIndex < 0 || recentIndex != jobsIndex+1 {
		t.Fatalf("job command ordering: /jobs=%d /jobs recent=%d", jobsIndex, recentIndex)
	}
}
