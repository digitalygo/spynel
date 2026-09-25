package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/shortid"
)

const maxJobMetadataRunes = 160

func (s *Service) jobsRecentCommand(message core.Message, emit core.Emit) error {
	items, err := s.Runtime.RecentArchivedJobs(jobArchiveRecentLimit)
	if err != nil {
		return s.localReply(message, "Recent job archives are unavailable: "+err.Error(), emit)
	}
	if len(items) == 0 {
		return s.localReply(message, "# Recent jobs\n\nNo archived jobs are available.", emit)
	}
	lines := []string{"# Recent jobs", "", fmt.Sprintf("Showing %d newest archived jobs by number.", len(items)), ""}
	for _, item := range items {
		when := item.StartedAt.UTC().Format(time.RFC3339)
		if !item.EndedAt.IsZero() {
			when = item.EndedAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, fmt.Sprintf("- **Job %d** %s  ", item.Number, safeJobText(item.Title, maxJobMetadataRunes)), fmt.Sprintf("  %s · %s · %s", when, safeJobText(item.State, 80), safeJobText(item.Origin, 80)))
	}
	lines = append(lines, "", "Use `/job info <number>` for metadata or `/job output <number>` for the newest output tail.")
	return s.localReply(message, strings.Join(lines, "\n"), emit)
}

func (s *Service) archivedJobInfoCommand(message core.Message, number int, emit core.Emit) error {
	item, _, err := s.Runtime.ArchivedJob(number)
	if err != nil {
		return s.localReply(message, fmt.Sprintf("Job %d was not found. Use `/jobs` or `/jobs recent`.", number), emit)
	}
	lines := []string{fmt.Sprintf("# Job %d", item.Number), "", "- Title: " + safeJobText(item.Title, maxJobMetadataRunes), "- Type: " + safeJobText(item.Kind, 80), "- Origin: " + safeJobText(item.Origin, 80), "- State: " + safeJobText(item.State, 80), "- Started: " + item.StartedAt.UTC().Format(time.RFC3339Nano)}
	if item.Provider != "" {
		lines = append(lines, "- Provider: "+safeJobText(item.Provider, 80))
	}
	if !item.EndedAt.IsZero() {
		lines = append(lines, "- Ended: "+item.EndedAt.UTC().Format(time.RFC3339Nano), "- Duration: "+item.EndedAt.Sub(item.StartedAt).Round(time.Millisecond).String())
	}
	lines = append(lines, "", fmt.Sprintf("Use `/job output %d` to inspect its bounded captured event stream.", item.Number))
	return s.localReply(message, strings.Join(lines, "\n"), emit)
}

func (s *Service) jobOutputCommand(message core.Message, number int, tailBytes int, emit core.Emit) error {
	var ref any = number
	if job, ok := s.Runtime.JobByNumber(number); ok {
		ref = job.StableID
	}
	item, output, err := s.Runtime.ArchivedJob(ref)
	if err != nil {
		return s.localReply(message, fmt.Sprintf("Job output %d was not found. Use `/jobs` or `/jobs recent`.", number), emit)
	}
	originalBytes := len(output)
	if len(output) > tailBytes {
		start := len(output) - tailBytes
		for start < len(output) && output[start]&0xc0 == 0x80 {
			start++
		}
		output = output[start:]
		output = "[EARLIER OUTPUT OMITTED]\n" + output
	}
	if strings.TrimSpace(output) == "" {
		output = "No captured provider output yet."
	}
	header := fmt.Sprintf("# Job output %d\n\nState: %s · showing newest %d of %d captured bytes.\n", item.Number, safeJobText(item.State, 80), min(originalBytes, tailBytes), originalBytes)
	return s.localReply(message, header+output, emit)
}

func (s *Service) jobInfoCommand(message core.Message, id int, emit core.Emit) error {
	job, ok := s.Runtime.JobByNumber(id)
	if !ok {
		return s.archivedJobInfoCommand(message, id, emit)
	}

	text := s.formatJobInfo(job)

	// Durable reads and harness completion can race. Never present a stale job
	// as active after it has left the process-local registry.
	current, stillRunning := s.Runtime.Job(job.ID)
	if !stillRunning || current.SessionKey != job.SessionKey {
		return s.localReply(message, fmt.Sprintf("Job %d finished while its details were being read. Use /jobs to list running jobs.", id), emit)
	}
	return s.localReply(message, text, emit)
}

func (s *Service) formatJobInfo(job Job) string {
	now := time.Now().UTC()
	kind := job.Kind
	if kind == "" {
		kind = "conversation"
	}
	route := job.Route
	if route == "" {
		route = job.Channel
	}

	lines := []string{
		fmt.Sprintf("# Job %d", publicJobNumber(job)), "",
		"- Kind: " + safeJobText(kind, maxJobMetadataRunes),
		"- Route: " + safeJobText(route, maxJobMetadataRunes),
		"- Execution status: " + safeJobText(formatExecutionStatus(job), maxJobMetadataRunes),
		"- Health: " + safeJobText(formatJobHealth(job), maxJobMetadataRunes),
		"- Current execution age: " + shortDuration(now.Sub(job.StartedAt)),
		"- Started: " + job.StartedAt.UTC().Format(time.RFC3339),
	}
	if job.Durable {
		first := job.FirstAssignedAt
		if first.IsZero() || first.After(now) {
			first = job.StartedAt
		}
		lines = append(lines,
			"- Durable lifetime: "+shortDuration(now.Sub(first)),
			fmt.Sprintf("- Provider steps (▶): %d", max(1, job.ProviderIterations)),
		)
	} else {
		lines = append(lines, "- Provider steps (▶): 1 (live conversation)")
	}
	if !job.LastActivityAt.IsZero() {
		age := now.Sub(job.LastActivityAt)
		if age < 0 {
			age = 0
		}
		lines = append(lines, "- Last activity: "+shortDuration(age)+" ago · "+job.LastActivityAt.UTC().Format(time.RFC3339))
	}
	if job.ReconnectAttempt > 0 {
		reconnect := fmt.Sprintf("%d", job.ReconnectAttempt)
		if job.ReconnectTotal > 0 {
			reconnect += fmt.Sprintf("/%d", job.ReconnectTotal)
		}
		lines = append(lines, "- Reconnect attempt: "+reconnect)
	}
	if job.StatusDetail != "" {
		lines = append(lines, "- Detail: "+safeJobText(boundJobStatusDetail(job.StatusDetail), maxJobMetadataRunes))
	}
	if thread := shortid.Display(s.Harness.ThreadID(job.SessionKey)); thread != "" {
		lines = append(lines, "- Execution: `"+safeJobText(thread, maxJobMetadataRunes)+"`")
	}
	return strings.Join(lines, "\n")
}

func safeJobText(value string, limit int) string {
	value = sanitizeLogText(value)
	value = strings.Join(strings.Fields(value), " ")
	replacer := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "\\<", ">", "\\>", "#", "\\#", "|", "\\|")
	return boundJobText(replacer.Replace(value), limit)
}

func boundJobText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
