package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/digitalygo/spynel/internal/app"
	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/channel/telegram"
	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/instance"
	"github.com/digitalygo/spynel/internal/localapi"
	"github.com/digitalygo/spynel/internal/theme"
	"github.com/digitalygo/spynel/internal/updater"
)

var errOwnershipLost = errors.New("workspace server ownership changed during startup")

type primaryOptions struct {
	Socket string
	Log    io.Writer
}

type primaryTerm struct {
	election *instance.Election
	token    string
	cancel   context.CancelFunc
	listener net.Listener
	service  *app.Service

	apiDone         chan struct{}
	apiError        chan error
	channelsDone    <-chan error
	replyQueueDone  chan struct{}
	maintenanceDone chan error
	stopOnce        sync.Once
}

func runOwnerElection(ctx context.Context, cfg config.Config, version string, election *instance.Election, restart func(), update func(updater.Result), options ...primaryOptions) error {
	ticker := time.NewTicker(instance.RetryInterval)
	defer ticker.Stop()
	var term *primaryTerm
	var nextHeartbeat time.Time
	defer func() {
		if term != nil {
			term.stop()
		}
	}()

	step := func() error {
		now := time.Now()
		if term != nil {
			select {
			case err := <-term.apiError:
				term.service.Runtime.LogEvent("error", "localapi", "stopped", "Workspace API stopped: "+errorText(err))
				term.stop()
				term = nil
				return nil
			default:
			}
			if now.Before(nextHeartbeat) {
				return nil
			}
			_, owned, err := election.Renew(term.token)
			if err != nil {
				term.service.Runtime.LogEvent("error", "instance", "lease_renew_failed", "Renew primary lease: "+err.Error())
			}
			if err != nil || !owned {
				term.stop()
				term = nil
				return nil
			}
			nextHeartbeat = now.Add(instance.HeartbeatInterval)
			return nil
		}

		current, err := election.Current()
		if err == nil && !election.CanTakeOver(current) {
			if len(options) > 0 && options[0].Socket != "" {
				return errors.New("--socket requires a new primary; an existing primary already owns this workspace")
			}
			return nil
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		token, err := election.NewToken()
		if err != nil {
			listener.Close()
			return err
		}
		_, acquired, err := election.TryAcquire(listener.Addr().String(), token)
		if err != nil || !acquired {
			listener.Close()
			return err
		}
		term, err = startPrimaryTerm(ctx, cfg, version, election, listener, token, restart, update, options...)
		if err != nil {
			_ = election.Release(token)
			listener.Close()
			if errors.Is(err, errOwnershipLost) {
				return nil
			}
			return err
		}
		nextHeartbeat = time.Now().Add(instance.HeartbeatInterval)
		return nil
	}

	if err := step(); err != nil {
		return err
	}
	for {
		var primaryRequests <-chan string
		if term != nil {
			primaryRequests = term.service.PrimaryRequests()
		}
		select {
		case <-ctx.Done():
			return nil
		case targetID := <-primaryRequests:
			term.service.Runtime.LogEvent("info", "instance", "handoff", "Handing primary ownership to instance "+targetID)
			if err := term.handoff(targetID); err != nil {
				term.service.Runtime.LogEvent("error", "instance", "handoff_failed", "Primary handoff: "+err.Error())
			}
			term = nil
		case <-ticker.C:
			if err := step(); err != nil {
				return err
			}
		}
	}
}

func startPrimaryTerm(parent context.Context, original config.Config, version string, election *instance.Election, listener net.Listener, token string, restart func(), update func(updater.Result), options ...primaryOptions) (*primaryTerm, error) {
	// A secondary may have waited for hours before taking over. Re-read YAML so
	// it never resurrects the snapshot from its own startup.
	cfg, err := config.Load(original.Path)
	if err != nil {
		runtimeState := app.NewRuntimeAt(original.StatePath("runtime", "logs"), election.ID()+"-reload")
		runtimeState.LogEvent("error", "config", "reload_failed", "Configuration reload before primary startup failed")
		runtimeState.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	service, err := buildService(cfg, version)
	if err != nil {
		cancel()
		return nil, err
	}
	var socket net.Listener
	if len(options) > 0 {
		service.Runtime.SetOperationalOutput(options[0].Log)
		if options[0].Socket != "" {
			socket, err = localapi.ListenSocket(options[0].Socket, cfg.Root, token)
			if err != nil {
				cancel()
				_ = service.Close()
				return nil, err
			}
		}
	}
	started := false
	defer func() {
		if !started && socket != nil {
			_ = socket.Close()
		}
	}()
	service.Runtime.LogEvent("info", "config", "reloaded", "Configuration loaded for primary startup")
	service.SetPrimaryInstanceID(election.ID())
	if err := service.Start(ctx); err != nil {
		service.Runtime.LogEvent("error", "harness", "unavailable", "Harness unavailable: "+err.Error())
	}
	if _, owned, renewErr := election.Renew(token); renewErr != nil || !owned {
		_ = service.Close()
		cancel()
		if renewErr != nil {
			return nil, renewErr
		}
		return nil, errOwnershipLost
	}
	service.Runtime.LogEvent("info", "runtime", "primary_started", "Spynel primary server started")
	term := &primaryTerm{
		election: election, token: token, cancel: cancel, listener: listener, service: service,
		apiDone: make(chan struct{}), apiError: make(chan error, 1), maintenanceDone: make(chan error, 1),
	}
	reportConnection := func(status channel.ConnectionStatus) { service.SetConnectionStatus(status) }
	// The durable final-reply worker belongs to the primary term, not to one
	// Telegram adapter generation: it runs for the whole owner lifetime even
	// while Telegram is disabled or disconnected, so expiry cleanup proceeds,
	// and it is joined before the primary lease is released. Every short
	// record mutation is fenced by this exact ownership term; provider
	// requests run outside the election lock.
	replyLog := service.Runtime.Writer("telegram.replies")
	replyWorker := telegram.NewReplyWorker(
		cfg.StatePath("runtime", "telegram-replies"),
		election.ID(),
		func(action func() error) (bool, error) { return election.RunWhileOwner(token, action) },
		func(line string) { _, _ = fmt.Fprintln(replyLog, line) },
	)
	term.replyQueueDone = make(chan struct{})
	go func() {
		defer service.Runtime.RecoverPanic("telegram.replies", "worker_panic")
		defer close(term.replyQueueDone)
		replyWorker.Run(ctx)
	}()
	term.channelsDone, err = startChannels(ctx, service, reportConnection, replyWorker)
	if err != nil {
		cancel()
		<-term.replyQueueDone
		_ = service.Close()
		return nil, err
	}
	apiServer := &localapi.Server{Service: service, Token: token}
	go func() {
		defer service.Runtime.RecoverPanic("localapi", "server_panic")
		listeners := []net.Listener{listener}
		if socket != nil {
			listeners = append(listeners, socket)
		}
		results := make(chan error, len(listeners))
		for _, target := range listeners {
			go func() { results <- apiServer.Serve(ctx, target) }()
		}
		for range listeners {
			err := <-results
			select {
			case term.apiError <- err:
			default:
			}
			cancel()
		}
		close(term.apiDone)
	}()

	go func() {
		defer service.Runtime.RecoverPanic("maintenance", "worker_panic")
		term.maintenanceDone <- runPrimaryMaintenance(ctx, service)
	}()
	go func() {
		defer service.Runtime.RecoverPanic("runtime", "request_panic")
		select {
		case <-ctx.Done():
		case <-service.RestartRequests():
			restart()
		case result := <-service.UpdateRequests():
			update(result)
		}
	}()
	started = true
	return term, nil
}

// runPrimaryMaintenance keeps the elected owner's notification retries and
// periodic cleanup running for the whole primary term, independent of whether
// a harness is currently available.
func runPrimaryMaintenance(ctx context.Context, service *app.Service) error {
	return service.RunPrimaryMaintenance(ctx)
}

func (term *primaryTerm) stop() {
	_ = term.stopFor("")
}

func (term *primaryTerm) handoff(targetID string) error {
	return term.stopFor(targetID)
}

func (term *primaryTerm) stopFor(targetID string) error {
	var stopErr error
	stopped := false
	term.stopOnce.Do(func() {
		defer term.service.Runtime.Close()
		stopped = true
		term.service.SetPrimaryInstanceID("")
		term.cancel()
		_ = term.listener.Close()
		_ = term.service.Harness.Close()
		<-term.apiDone
		<-term.channelsDone
		<-term.replyQueueDone
		<-term.maintenanceDone
		if targetID != "" {
			_, handedOff, err := term.election.Handoff(term.token, targetID)
			if err == nil && handedOff {
				return
			}
			if err == nil {
				err = errOwnershipLost
			}
			if releaseErr := term.election.Release(term.token); releaseErr != nil {
				err = errors.Join(err, releaseErr)
			}
			stopErr = err
			return
		}
		if err := term.election.Release(term.token); err != nil {
			term.service.Runtime.LogEvent("error", "instance", "lease_release_failed", "Release primary lease: "+err.Error())
		}
	})
	if !stopped && targetID != "" {
		return errOwnershipLost
	}
	return stopErr
}

func errorText(err error) string {
	if err == nil {
		return "without an error"
	}
	return err.Error()
}

type tuiStateEvents struct {
	connections chan channel.ConnectionStatus
	pairings    chan channel.PairingEvent
	notices     chan channel.Notice
	titles      chan string
	themes      chan theme.Theme
	runtime     chan core.RuntimeStatus
}

func startTUIStatePolling(ctx context.Context, client *localapi.Client, initial app.SharedState, themeDirectory string) tuiStateEvents {
	events := tuiStateEvents{
		connections: make(chan channel.ConnectionStatus, 4),
		pairings:    make(chan channel.PairingEvent, 4),
		notices:     make(chan channel.Notice, 4), titles: make(chan string, 1),
		themes: make(chan theme.Theme, 1), runtime: make(chan core.RuntimeStatus, 1),
	}
	go func() {
		previous := initial
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			state, err := client.State(ctx)
			if err != nil {
				continue
			}
			publishTUIStateChanges(events, previous, state, themeDirectory)
			previous = state
		}
	}()
	return events
}

func publishTUIStateChanges(events tuiStateEvents, previous, state app.SharedState, themeDirectory string) {
	if state.Title != previous.Title {
		publishLatest(events.titles, state.Title)
	}
	if state.Theme != previous.Theme {
		if values, err := theme.LoadDir(themeDirectory); err == nil {
			if selected, ok := theme.Find(values, state.Theme); ok {
				publishLatest(events.themes, selected)
			}
		}
	}
	previousConnections := connectionStateMap(previous.Connections)
	for _, status := range state.Connections {
		if old, ok := previousConnections[status.Name]; !ok || old != status {
			publishLatest(events.connections, status)
		}
	}
	previousPairings := pairingStateMap(previous.Pairings)
	for _, pairing := range state.Pairings {
		if old, ok := previousPairings[pairing.Name]; !ok || old != pairing {
			publishLatest(events.pairings, pairing)
		}
	}
	if state.Runtime != previous.Runtime {
		publishLatest(events.runtime, state.Runtime)
	}
	if state.NoticeSequence != previous.NoticeSequence && state.Notice.Channel != "" {
		publishLatest(events.notices, state.Notice)
	}
}

func connectionStateMap(values []channel.ConnectionStatus) map[string]channel.ConnectionStatus {
	result := make(map[string]channel.ConnectionStatus, len(values))
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}

func pairingStateMap(values []channel.PairingEvent) map[string]channel.PairingEvent {
	result := make(map[string]channel.PairingEvent, len(values))
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}

func publishLatest[T any](target chan T, value T) {
	select {
	case <-target:
	default:
	}
	select {
	case target <- value:
	default:
	}
}
