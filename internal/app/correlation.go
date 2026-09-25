package app

import (
	"fmt"
	"sort"

	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/history"
	"github.com/digitalygo/spynel/internal/shortid"
)

// executionCorrelation is the process-local identity shared by every
// admission that belongs to one provider execution for a conversation.
type executionCorrelation struct {
	id      string
	sources map[string]struct{}
}

// correlationReservation records one admission's contribution to an
// execution correlation so a pre-provider failure can roll it back.
type correlationReservation struct {
	executionID string
	sourceID    string
	created     bool
	sourceAdded bool
}

func (s *Service) reserveExecutionCorrelation(message core.Message) (correlationReservation, string, error) {
	s.correlationMu.Lock()
	defer s.correlationMu.Unlock()
	key := sessionKey(message)
	current := s.correlations[key]
	reservation := correlationReservation{sourceID: message.SourceMessageID}
	admission := "new"
	if provider, ok := s.Harness.(interface{ ConversationAdmission(string) string }); ok {
		if value := provider.ConversationAdmission(key); value == "new" || value == "queued" || value == "steered" {
			admission = value
		}
	}
	if current == nil {
		id, err := shortid.New()
		if err != nil {
			return correlationReservation{}, "", fmt.Errorf("create execution correlation: %w", err)
		}
		current = &executionCorrelation{id: id, sources: map[string]struct{}{}}
		s.correlations[key] = current
		reservation.created = true
	} else if admission == "new" {
		admission = "followup"
	}
	reservation.executionID = current.id
	if message.SourceMessageID != "" {
		if _, exists := current.sources[message.SourceMessageID]; !exists {
			current.sources[message.SourceMessageID] = struct{}{}
			reservation.sourceAdded = true
		}
	}
	_, err := s.History.Append(message.Channel, message.Conversation, history.Entry{
		Role: "correlation", SourceMessageID: message.SourceMessageID,
		ExecutionID: current.id, Admission: admission,
	})
	if err != nil {
		s.rollbackCorrelationReservationLocked(key, reservation)
		return correlationReservation{}, "", err
	}
	return reservation, admission, nil
}

func (s *Service) rollbackCorrelationReservation(message core.Message, reservation correlationReservation) {
	s.correlationMu.Lock()
	defer s.correlationMu.Unlock()
	s.rollbackCorrelationReservationLocked(sessionKey(message), reservation)
}

func (s *Service) rollbackCorrelationReservationLocked(key string, reservation correlationReservation) {
	current := s.correlations[key]
	if current == nil || current.id != reservation.executionID {
		return
	}
	if reservation.sourceAdded {
		delete(current.sources, reservation.sourceID)
	}
	if reservation.created && len(current.sources) == 0 {
		delete(s.correlations, key)
	}
}

func (s *Service) finishExecutionCorrelation(message core.Message, outcome string) {
	key := sessionKey(message)
	s.correlationMu.Lock()
	current := s.correlations[key]
	delete(s.correlations, key)
	s.correlationMu.Unlock()
	if current == nil {
		return
	}
	covers := make([]string, 0, len(current.sources))
	for source := range current.sources {
		covers = append(covers, source)
	}
	sort.Strings(covers)
	if _, err := s.History.Append(message.Channel, message.Conversation, history.Entry{
		Role: "correlation", ExecutionID: current.id, Covers: covers, Outcome: outcome,
	}); err != nil {
		s.Runtime.LogEvent("error", "history", "terminal_correlation_failed", "Terminal conversation correlation could not be persisted")
	}
}

func (s *Service) correlationCancellationSnapshot(key string) *executionCorrelation {
	s.correlationMu.Lock()
	defer s.correlationMu.Unlock()
	current := s.correlations[key]
	if current == nil {
		return nil
	}
	snapshot := &executionCorrelation{id: current.id, sources: map[string]struct{}{}}
	for source := range current.sources {
		snapshot.sources[source] = struct{}{}
	}
	return snapshot
}

func (s *Service) commitCorrelationCancellation(key, channelName, conversation string, snapshot *executionCorrelation) {
	if snapshot == nil {
		return
	}
	s.correlationMu.Lock()
	if current := s.correlations[key]; current != nil && current.id == snapshot.id {
		delete(s.correlations, key)
	}
	s.correlationMu.Unlock()
	covers := make([]string, 0, len(snapshot.sources))
	for source := range snapshot.sources {
		covers = append(covers, source)
	}
	sort.Strings(covers)
	_, _ = s.History.Append(channelName, conversation, history.Entry{Role: "correlation", ExecutionID: snapshot.id, Covers: covers, Outcome: "intentional_cancellation"})
}
