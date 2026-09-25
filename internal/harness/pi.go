package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/fsx"
)

const piRPCMaxRecord = 16 * 1024 * 1024

// Pi is a native adapter for Pi's documented JSONL RPC mode. Pi owns one
// current session per process, so Spynel keeps one idle-capable process per
// active conversation and persists its session file for restart/resume.
type Pi struct {
	config HarnessConfig

	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	processes    map[string]*piProcess
	sessions     map[string]piSession
	keyMu        sync.Mutex
	keyLocks     map[string]*sync.Mutex
	modelCatalog []Model
}

type piSession struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Policy string `json:"policy"`
}

type piProcess struct {
	owner  *Pi
	key    string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser

	writeMu       sync.Mutex
	mu            sync.Mutex
	nextID        uint64
	pending       map[string]chan piResponse
	active        *piTurn
	session       piSession
	modelID       string
	thinkingLevel string
	closed        bool
}

type piTurn struct {
	emit core.Emit

	mu                 sync.Mutex
	text               strings.Builder
	currentMessage     strings.Builder
	successfulMessages []string
	lastMessage        string
	assistantOpen      bool
	errorText          string
	completed          bool
	deliveryMu         sync.Mutex
}

type piResponse struct {
	Data  json.RawMessage
	Error error
}

type piWireMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command,omitempty"`
	Success bool            `json:"success,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type piState struct {
	SessionFile   string `json:"sessionFile"`
	SessionID     string `json:"sessionId"`
	SessionName   string `json:"sessionName"`
	IsStreaming   bool   `json:"isStreaming"`
	ThinkingLevel string `json:"thinkingLevel"`
	Model         struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	} `json:"model"`
}

func NewPi(cfg HarnessConfig) (*Pi, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		cfg.Command = "pi"
	}
	if strings.TrimSpace(cfg.Cwd) == "" {
		cfg.Cwd = "."
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "danger-full-access"
	}
	adapter := &Pi{
		config: cfg, processes: map[string]*piProcess{}, sessions: map[string]piSession{}, keyLocks: map[string]*sync.Mutex{},
	}
	if err := adapter.loadSessions(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func (p *Pi) FollowUpMode() FollowUpMode { return FollowUpSteer }

// NativeConversationInput reports whether an ordinary send for key delivers
// the raw accepted conversation text that Pi expands with its own native
// skills, prompt templates, and extension commands. Only session keys
// matching the ordinary `chat:<channel>:<conversation>` grammar qualify;
// control and unknown keys report false so the caller keeps
// the provider-neutral bounded-context prompt.
func (p *Pi) NativeConversationInput(key string) bool {
	return piOrdinaryConversationKey(key)
}

// piOrdinaryConversationKey reports whether key matches the provider-neutral
// conversation grammar `chat:<channel>:<conversation>` with nonempty channel
// and conversation segments. All other keys report false.
func piOrdinaryConversationKey(key string) bool {
	rest, ok := strings.CutPrefix(key, "chat:")
	if !ok {
		return false
	}
	channel, conversation, ok := strings.Cut(rest, ":")
	return ok && channel != "" && conversation != "" && !strings.Contains(conversation, ":")
}

// piTelegramConversationKey reports whether key is an ordinary conversation
// on the telegram channel, matching the exact `chat:telegram:<conversation>`
// grammar with a nonempty conversation segment.
func piTelegramConversationKey(key string) bool {
	conversation, ok := strings.CutPrefix(key, "chat:telegram:")
	return ok && conversation != "" && !strings.Contains(conversation, ":")
}

func (p *Pi) Start(parent context.Context) error {
	p.mu.Lock()
	if p.ctx != nil {
		p.mu.Unlock()
		return nil
	}
	if p.closed {
		p.mu.Unlock()
		return errors.New("Pi harness is closed")
	}
	info, err := os.Stat(p.config.Cwd)
	if err != nil || !info.IsDir() {
		p.mu.Unlock()
		return fmt.Errorf("Pi working directory %q is unavailable", p.config.Cwd)
	}
	p.ctx, p.cancel = context.WithCancel(parent)
	checkContext, cancelCheck := context.WithTimeout(p.ctx, 5*time.Second)
	p.mu.Unlock()
	defer cancelCheck()

	output := newTailBuffer(16 * 1024)
	command := exec.CommandContext(checkContext, p.config.Command, "--version")
	command.Dir = p.config.Cwd
	command.Env = piProcessEnvironment(p.config.Env)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		_ = p.Close()
		return fmt.Errorf("Pi executable %q failed --version capability check: %w (%s)", p.config.Command, err, strings.TrimSpace(output.String()))
	}
	if strings.TrimSpace(output.String()) == "" {
		_ = p.Close()
		return fmt.Errorf("Pi executable %q returned an incompatible empty --version result", p.config.Command)
	}
	return nil
}

func (p *Pi) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	p.mu.Lock()
	selection := InferenceSelection{Model: p.config.Model, Effort: p.config.Effort, LegacyEffort: p.config.LegacyEffort}
	p.mu.Unlock()
	return p.SendWithInference(ctx, key, prompt, selection, emit)
}

func (p *Pi) SetModel(model string) {
	p.mu.Lock()
	p.config.Model = model
	p.mu.Unlock()
}

func (p *Pi) SetInference(selection InferenceSelection) {
	p.mu.Lock()
	p.config.Model, p.config.Effort, p.config.LegacyEffort = selection.Model, selection.Effort, selection.LegacyEffort
	p.mu.Unlock()
}

func (p *Pi) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	p.mu.Lock()
	selection := InferenceSelection{Model: model, Effort: p.config.Effort, LegacyEffort: p.config.LegacyEffort}
	p.mu.Unlock()
	return p.SendWithInference(ctx, key, prompt, selection, emit)
}

func (p *Pi) SendWithInference(ctx context.Context, key, prompt string, selection InferenceSelection, emit core.Emit) (string, bool, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", false, errors.New("harness prompt is empty")
	}
	if selection.ServiceMode != "" {
		return "", false, errors.New("Pi does not support a Spynel service mode; reset harness.service_mode to inherit")
	}
	if err := ValidateInferenceSelection(nil, selection); err != nil {
		return "", false, err
	}
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	process, err := p.ensureProcess(ctx, key, selection.Model, selection.Effort)
	if err != nil {
		return "", false, err
	}
	process.mu.Lock()
	active := process.active
	process.mu.Unlock()
	if active != nil {
		// Raw slash input keeps Pi's own native expansion during an active
		// turn: Pi rejects extension commands on steer, so recognized
		// extension commands, skills, prompt templates, and unknown slash
		// names all travel through prompt with streamingBehavior "steer".
		native := strings.HasPrefix(prompt, "/")
		threadID, err := p.steerLocked(ctx, process, prompt, emit, nil, native)
		return threadID, true, err
	}
	turn := &piTurn{emit: emit}
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return process.session.ID, false, errors.New("Pi RPC process is closed")
	}
	process.active = turn
	process.mu.Unlock()
	_, err = process.call(ctx, map[string]any{"type": "prompt", "message": prompt}, nil)
	if err != nil {
		process.mu.Lock()
		if process.active == turn {
			process.active = nil
		}
		process.mu.Unlock()
		return process.session.ID, false, fmt.Errorf("Pi rejected prompt: %w", err)
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventStatus, Text: "Pi turn started", ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "running"}})
	}
	if name, ok := piNativeCommandName(prompt); ok && process.extensionCommandRecognized(ctx, name) {
		process.settleNativeCommandWithoutRun(ctx, turn, name)
	}
	return process.session.ID, false, nil
}

// piNativeCommandName reports the native command name for raw slash input
// using Pi's exact parsing: the token between the leading slash and the
// first space. Non-slash input never names a command.
func piNativeCommandName(prompt string) (string, bool) {
	if !strings.HasPrefix(prompt, "/") {
		return "", false
	}
	rest := prompt[1:]
	if index := strings.Index(rest, " "); index >= 0 {
		return rest[:index], true
	}
	return rest, true
}

func (p *Pi) Steer(ctx context.Context, key, prompt string, emit core.Emit, beforeDelivery func() bool) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("harness prompt is empty")
	}
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	process := p.processes[key]
	p.mu.Unlock()
	if process == nil {
		return p.ThreadID(key), fmt.Errorf("Pi turn is no longer active: %w", errNativeTurnInactive)
	}
	// Control-plane deliveries keep the strict steer command so delimited
	// coordination text is never interpreted as a native command.
	return p.steerLocked(ctx, process, prompt, emit, beforeDelivery, false)
}

// steerLocked delivers a follow-up into the turn that is already active for
// the process. Ordinary text uses Pi's steer command. Raw slash input sets
// native because Pi rejects extension commands on steer and instead executes
// recognized extension commands immediately, expands skills and prompt
// templates, and queues unknown slash names, all through prompt with
// streamingBehavior "steer". Delivery order, the durable iteration
// reservation, and the emitter transfer stay identical for both forms.
func (p *Pi) steerLocked(ctx context.Context, process *piProcess, prompt string, emit core.Emit, beforeDelivery func() bool, native bool) (string, error) {
	process.mu.Lock()
	turn := process.active
	process.mu.Unlock()
	if turn == nil {
		return process.session.ID, fmt.Errorf("Pi turn is no longer active: %w", errNativeTurnInactive)
	}
	turn.deliveryMu.Lock()
	defer turn.deliveryMu.Unlock()
	turn.mu.Lock()
	completed := turn.completed
	turn.mu.Unlock()
	if completed {
		return process.session.ID, fmt.Errorf("Pi turn is no longer active: %w", errNativeTurnInactive)
	}
	var previous core.Emit
	reserved := false
	message := map[string]any{"type": "steer", "message": prompt}
	if native {
		message = map[string]any{"type": "prompt", "message": prompt, "streamingBehavior": "steer"}
	}
	_, err := process.call(ctx, message, func() bool {
		if beforeDelivery != nil && !beforeDelivery() {
			return false
		}
		reserved = true
		turn.mu.Lock()
		previous = turn.emit
		turn.emit = emit
		turn.mu.Unlock()
		return true
	})
	if err != nil {
		turn.mu.Lock()
		if !turn.completed {
			turn.emit = previous
		}
		turn.mu.Unlock()
		if !reserved && beforeDelivery != nil {
			return process.session.ID, errNativeDeliveryUnreserved
		}
		return process.session.ID, err
	}
	if previous != nil {
		previous(core.Event{Kind: core.EventStatus, Text: "Response continued on a newer message", ThreadID: process.session.ID, Done: true})
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventStatus, Text: "Steering active Pi turn", ThreadID: process.session.ID})
	}
	return process.session.ID, nil
}

func (p *Pi) Interrupt(ctx context.Context, key string) (bool, error) {
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	process := p.processes[key]
	p.mu.Unlock()
	if process == nil {
		return false, nil
	}
	process.mu.Lock()
	active := process.active != nil
	process.mu.Unlock()
	if !active {
		return false, nil
	}
	if _, err := process.call(ctx, map[string]any{"type": "abort"}, nil); err != nil {
		return false, fmt.Errorf("abort Pi turn: %w", err)
	}
	return true, nil
}

func (p *Pi) ResetSession(key string) error {
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	process := p.processes[key]
	if process != nil {
		process.mu.Lock()
		active := process.active != nil
		process.mu.Unlock()
		if active {
			p.mu.Unlock()
			return errors.New("cannot reset a Pi session while its turn is active")
		}
		delete(p.processes, key)
	}
	delete(p.sessions, key)
	err := p.saveSessionsLocked()
	p.mu.Unlock()
	if process != nil {
		process.close()
	}
	return err
}

func (p *Pi) ThreadID(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[key].ID
}

func (p *Pi) IsActive(key string) bool {
	p.mu.Lock()
	process := p.processes[key]
	p.mu.Unlock()
	if process == nil {
		return false
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.active != nil
}

// ProvidesConversationContext reports whether an ordinary send for key would
// reuse a provider session that already retains this conversation. It runs
// under the same per-key lock and policy computation as ensureProcess so the
// capability answer and the dispatch path cannot drift. Any uncertainty,
// closed process, missing session, stale policy, or unavailable session file
// reports false so the caller safely seeds bounded history instead.
func (p *Pi) ProvidesConversationContext(key string) bool {
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return false
	}
	process := p.processes[key]
	session, sessionOK := p.sessions[key]
	policy := p.sessionPolicyLocked(p.config.Model, p.config.Effort)
	p.mu.Unlock()
	if process != nil {
		process.mu.Lock()
		active := process.active != nil
		closed := process.closed
		processSession := process.session
		process.mu.Unlock()
		if closed {
			return false
		}
		if active {
			// An active turn holds the conversation in provider memory even when
			// a configuration commit changed the session policy.
			return processSession.ID != "" && processSession.Path != ""
		}
		if processSession.Policy != policy {
			return false
		}
		// A live idle process still retains the context even if its persisted
		// session file was removed.
		return true
	}
	if !sessionOK || session.ID == "" || session.Path == "" || session.Policy != policy {
		return false
	}
	info, err := os.Stat(session.Path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return true
}

func (p *Pi) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cancel := p.cancel
	processes := make([]*piProcess, 0, len(p.processes))
	for _, process := range p.processes {
		processes = append(processes, process)
	}
	p.processes = map[string]*piProcess{}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, process := range processes {
		process.close()
	}
	return nil
}

func (p *Pi) Models(ctx context.Context) ([]Model, error) {
	p.mu.Lock()
	if p.modelCatalog != nil {
		models := append([]Model(nil), p.modelCatalog...)
		p.mu.Unlock()
		return models, nil
	}
	p.mu.Unlock()
	process, err := p.startProcess(ctx, "", piSession{}, true, "", "")
	if err != nil {
		return nil, err
	}
	defer process.close()
	data, err := process.call(ctx, map[string]any{"type": "get_available_models"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list Pi models: %w", err)
	}
	var response struct {
		Models []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Provider  string `json:"provider"`
			Reasoning bool   `json:"reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode Pi model catalog: %w", err)
	}
	models := make([]Model, 0, len(response.Models))
	for _, item := range response.Models {
		id := item.ID
		if item.Provider != "" && !strings.Contains(id, "/") {
			id = item.Provider + "/" + id
		}
		if id == "" {
			continue
		}
		model := Model{ID: id, DisplayName: item.Name, Default: id == process.modelID}
		if model.DisplayName == "" {
			model.DisplayName = id
		}
		// Pi's set_model RPC persists the selected model as Pi's global default.
		// Probe each model in its own no-session process instead: the --model CLI
		// option is a runtime override and does not mutate Pi's settings.
		modelProcess, err := p.startProcess(ctx, "", piSession{}, true, id, "")
		if err != nil {
			return nil, fmt.Errorf("start Pi capability discovery for model %q: %w", id, err)
		}
		levelsData, err := modelProcess.call(ctx, map[string]any{"type": "get_available_thinking_levels"}, nil)
		modelProcess.close()
		if err != nil {
			return nil, fmt.Errorf("list Pi thinking levels for model %q: %w", id, err)
		}
		var levelsResponse struct {
			Levels []string `json:"levels"`
		}
		if err := json.Unmarshal(levelsData, &levelsResponse); err != nil {
			return nil, fmt.Errorf("decode Pi thinking levels for model %q: %w", id, err)
		}
		// Pi documents ["off"] as the sentinel for a model without reasoning.
		// Such a model should keep the dependent selection flow short.
		if !(len(levelsResponse.Levels) == 1 && levelsResponse.Levels[0] == "off") {
			model.Efforts = append([]string(nil), levelsResponse.Levels...)
			if model.Default && containsString(model.Efforts, process.thinkingLevel) {
				model.DefaultEffort = process.thinkingLevel
			}
		}
		models = append(models, model)
	}
	p.mu.Lock()
	p.modelCatalog = append([]Model(nil), models...)
	p.mu.Unlock()
	return models, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (p *Pi) ensureProcess(ctx context.Context, key, model, effort string) (*piProcess, error) {
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return nil, errors.New("Pi harness is not running")
	}
	if process := p.processes[key]; process != nil {
		process.mu.Lock()
		active := process.active != nil
		policyMatches := process.session.Policy == p.sessionPolicyLocked(model, effort)
		process.mu.Unlock()
		if active || policyMatches {
			p.mu.Unlock()
			return process, nil
		}
		delete(p.processes, key)
		p.mu.Unlock()
		process.close()
		p.mu.Lock()
	}
	session := p.sessions[key]
	if session.Policy != p.sessionPolicyLocked(model, effort) {
		session = piSession{}
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	process, err := p.startProcess(ctx, key, session, false, model, effort)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		process.close()
		return nil, errors.New("Pi harness closed while starting a session")
	}
	if existing := p.processes[key]; existing != nil {
		p.mu.Unlock()
		process.close()
		return existing, nil
	}
	p.processes[key] = process
	p.mu.Unlock()
	return process, nil
}

// sessionDirectory is the workspace-local store for Spynel conversation Pi
// sessions. It lives beside the harness session map so restart recovery and
// import cleanup share one exact path.
func (p *Pi) sessionDirectory(cfg HarnessConfig) string {
	if cfg.SessionsFile == "" {
		return filepath.Join(cfg.Cwd, ".spynel", "runtime", "pi-sessions")
	}
	return filepath.Join(filepath.Dir(cfg.SessionsFile), "pi-sessions")
}

func (p *Pi) startProcess(ctx context.Context, key string, session piSession, ephemeral bool, model, effort string) (*piProcess, error) {
	return p.startProcessValidated(ctx, key, session, ephemeral, model, effort, nil)
}

func (p *Pi) startProcessValidated(ctx context.Context, key string, session piSession, ephemeral bool, model, effort string, validate func(piState) error) (*piProcess, error) {
	p.mu.Lock()
	cfg := p.config
	p.mu.Unlock()
	cfg.Model = model
	cfg.Effort = effort
	args := []string{"--mode", "rpc"}
	if ephemeral {
		args = append(args, "--no-session")
	} else {
		sessionDir := p.sessionDirectory(cfg)
		if err := os.MkdirAll(sessionDir, 0o700); err != nil {
			return nil, err
		}
		args = append(args, "--session-dir", sessionDir)
		if session.Path != "" {
			if _, err := os.Stat(session.Path); err == nil {
				args = append(args, "--session", session.Path)
			}
		}
	}
	return p.startProcessArgs(ctx, key, args, cfg, !ephemeral, !ephemeral, validate)
}

// startProcessArgs launches one Pi RPC process from explicit arguments.
// requireSession enforces a persisted session identity from get_state, and
// persist records that identity in the durable session map before returning.
// validate runs after negotiation and before persistence so control
// operations can fail closed without storing an unexpected session.
func (p *Pi) startProcessArgs(ctx context.Context, key string, args []string, cfg HarnessConfig, requireSession, persist bool, validate func(piState) error) (*piProcess, error) {
	p.mu.Lock()
	baseContext := p.ctx
	closed := p.closed
	p.mu.Unlock()
	if !requireSession {
		// Ephemeral capability discovery uses the caller context.
		baseContext = ctx
	}
	if closed || baseContext == nil {
		return nil, errors.New("Pi harness is not running")
	}
	processContext, cancel := context.WithCancel(baseContext)
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--thinking", cfg.Effort)
	}
	if cfg.Sandbox == "read-only" {
		args = append(args, "--tools", "read,grep,find,ls")
	}
	// Telegram conversations see final replies only, so every process launch
	// for a chat:telegram key loads exactly one Spynel-authored Pi extension
	// additively through --extension. On before_agent_start the extension adds
	// one namespaced system prompt section, so Pi keeps discovering the user's
	// global and trusted-project APPEND_SYSTEM.md, and ordinary global
	// extensions, skills, templates, themes, and context files stay untouched.
	// Ephemeral discovery processes and every other channel never load it.
	if piTelegramConversationKey(key) {
		extensionPath, err := ensurePiTelegramNoteExtension(cfg)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("prepare the Telegram note extension: %w", err)
		}
		args = append(args, "--extension", extensionPath)
	}
	command := exec.CommandContext(processContext, cfg.Command, args...)
	command.Dir = cfg.Cwd
	command.Env = piProcessEnvironment(cfg.Env)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if cfg.Stderr != nil {
		command.Stderr = cfg.Stderr
	}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Pi RPC process: %w", err)
	}
	process := &piProcess{owner: p, key: key, cmd: command, cancel: cancel, stdin: stdin, nextID: 1, pending: map[string]chan piResponse{}}
	go process.readLoop(stdout)
	go process.waitLoop()
	data, err := process.call(ctx, map[string]any{"type": "get_state"}, nil)
	if err != nil {
		process.close()
		return nil, fmt.Errorf("Pi executable %q failed RPC get_state negotiation: %w", cfg.Command, err)
	}
	var state piState
	if err := json.Unmarshal(data, &state); err != nil || (requireSession && (state.SessionID == "" || state.SessionFile == "")) {
		process.close()
		return nil, fmt.Errorf("Pi executable %q returned an incompatible get_state result with missing sessionId or sessionFile", cfg.Command)
	}
	if validate != nil {
		if err := validate(state); err != nil {
			process.close()
			return nil, err
		}
	}
	process.mu.Lock()
	process.session = piSession{ID: state.SessionID, Path: state.SessionFile, Policy: piSessionPolicy(cfg)}
	process.modelID = piCatalogModelID(state.Model.Provider, state.Model.ID)
	process.thinkingLevel = state.ThinkingLevel
	process.mu.Unlock()
	if _, err := process.call(ctx, map[string]any{"type": "set_steering_mode", "mode": "all"}, nil); err != nil {
		process.close()
		return nil, fmt.Errorf("configure Pi steering queue: %w", err)
	}
	if _, err := process.call(ctx, map[string]any{"type": "set_follow_up_mode", "mode": "all"}, nil); err != nil {
		process.close()
		return nil, fmt.Errorf("configure Pi follow-up queue: %w", err)
	}
	if persist {
		p.mu.Lock()
		p.sessions[key] = process.session
		err = p.saveSessionsLocked()
		p.mu.Unlock()
		if err != nil {
			process.close()
			return nil, err
		}
	}
	return process, nil
}

func piCatalogModelID(provider, id string) string {
	if provider != "" && id != "" && !strings.Contains(id, "/") {
		return provider + "/" + id
	}
	return id
}

func (process *piProcess) call(ctx context.Context, message map[string]any, beforeWrite func() bool) (json.RawMessage, error) {
	process.writeMu.Lock()
	process.mu.Lock()
	if process.closed || process.stdin == nil {
		process.mu.Unlock()
		process.writeMu.Unlock()
		return nil, errors.New("Pi RPC stdin is closed")
	}
	id := "spynel-" + strconv.FormatUint(process.nextID, 10)
	process.nextID++
	waiter := make(chan piResponse, 1)
	process.pending[id] = waiter
	stdin := process.stdin
	process.mu.Unlock()
	if beforeWrite != nil && !beforeWrite() {
		process.mu.Lock()
		delete(process.pending, id)
		process.mu.Unlock()
		process.writeMu.Unlock()
		return nil, errNativeDeliveryUnreserved
	}
	message["id"] = id
	data, err := json.Marshal(message)
	if err == nil {
		_, err = stdin.Write(append(data, '\n'))
	}
	if err != nil {
		process.mu.Lock()
		delete(process.pending, id)
		process.mu.Unlock()
	}
	process.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case response := <-waiter:
		return response.Data, response.Error
	case <-ctx.Done():
		process.mu.Lock()
		delete(process.pending, id)
		process.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (process *piProcess) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), piRPCMaxRecord)
	for scanner.Scan() {
		var envelope piWireMessage
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			process.fail(fmt.Errorf("Pi RPC emitted incompatible non-JSON output: %w", err))
			return
		}
		if envelope.Type == "extension_ui_request" {
			process.answerExtensionUI(scanner.Bytes())
			continue
		}
		if envelope.Type == "response" {
			process.mu.Lock()
			waiter := process.pending[envelope.ID]
			delete(process.pending, envelope.ID)
			process.mu.Unlock()
			if waiter != nil {
				response := piResponse{Data: envelope.Data}
				if !envelope.Success {
					response.Error = errors.New(emptyPiError(envelope.Error))
				}
				waiter <- response
			}
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &event); err == nil {
			process.handleEvent(envelope.Type, event)
		}
	}
	process.fail(fmt.Errorf("Pi RPC stream closed: %v", scanner.Err()))
}

// answerExtensionUI fails closed on Pi extension dialogs so a headless turn
// can never wait on interactive UI. Blocking dialogs receive an immediate
// explicit cancellation; fire-and-forget methods expect no response. The
// cancellation is surfaced through the active turn's provider-neutral status.
func (process *piProcess) answerExtensionUI(raw []byte) {
	var request struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.ID == "" {
		return
	}
	switch request.Method {
	case "confirm", "select", "input", "editor":
	default:
		return
	}
	response, err := json.Marshal(map[string]any{"type": "extension_ui_response", "id": request.ID, "cancelled": true})
	if err != nil {
		return
	}
	process.writeMu.Lock()
	process.mu.Lock()
	stdin := process.stdin
	closed := process.closed
	process.mu.Unlock()
	if !closed && stdin != nil {
		_, _ = stdin.Write(append(response, '\n'))
	}
	process.writeMu.Unlock()
	process.mu.Lock()
	turn := process.active
	threadID := process.session.ID
	process.mu.Unlock()
	if turn != nil {
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi extension dialog cancelled: no interactive extension UI bridge", ThreadID: threadID})
	}
}

func emptyPiError(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Pi RPC command failed"
	}
	return value
}

// extensionCommandRecognized reports whether the live Pi session registers an
// extension command with the exact name, so a raw slash dispatch is known to
// execute immediately inside Pi instead of queueing an ordinary prompt. Any
// query failure reports false so dispatch keeps the ordinary event-driven
// lifecycle.
func (process *piProcess) extensionCommandRecognized(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	data, err := process.call(ctx, map[string]any{"type": "get_commands"}, nil)
	if err != nil {
		return false
	}
	var response struct {
		Commands []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false
	}
	for _, command := range response.Commands {
		if command.Name == name && command.Source == "extension" {
			return true
		}
	}
	return false
}

// settleNativeCommandWithoutRun ends a turn that Pi accepted as a recognized
// native extension command. Pi awaits the command handler before the prompt
// response, and an accepted ordinary prompt is already streaming when the
// client sees that response, so a following get_state reporting an idle
// session is positive evidence that the command was handled without a model
// run and no agent_settled will ever arrive. The settlement is deliberately
// honest: one plain status and the ordinary empty final, never a fabricated
// model answer derived from the RPC acknowledgment. A handler that started a
// run keeps ordinary agent_settled settlement authoritative, including runs
// that already settled the turn before the response arrived.
func (process *piProcess) settleNativeCommandWithoutRun(ctx context.Context, turn *piTurn, name string) {
	data, err := process.call(ctx, map[string]any{"type": "get_state"}, nil)
	if err != nil {
		return
	}
	var state piState
	if err := json.Unmarshal(data, &state); err != nil || state.IsStreaming {
		return
	}
	process.mu.Lock()
	stillActive := process.active == turn
	process.mu.Unlock()
	if !stillActive {
		return
	}
	turn.emitEvent(core.Event{Kind: core.EventStatus, Text: fmt.Sprintf("Pi handled native command /%s without a model run", name), ThreadID: process.session.ID})
	process.finishTurn(turn)
}

func (process *piProcess) handleEvent(kind string, event map[string]json.RawMessage) {
	process.mu.Lock()
	turn := process.active
	process.mu.Unlock()
	if turn == nil {
		return
	}
	switch kind {
	case "message_start":
		var message struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(event["message"], &message)
		if message.Role == "assistant" {
			turn.startAssistant(process.session.ID)
		}
	case "message_update":
		var update struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(event["assistantMessageEvent"], &update)
		if update.Type == "text_delta" && update.Delta != "" {
			turn.appendText(process.session.ID, update.Delta)
		}
	case "message_end":
		var message struct {
			Role         string          `json:"role"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
			Content      json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(event["message"], &message)
		if message.Role == "assistant" {
			if message.ErrorMessage != "" || message.StopReason == "error" {
				errorText := message.ErrorMessage
				if errorText == "" {
					errorText = "Pi assistant message failed"
				}
				turn.failMessage(process.session.ID, piMessageText(message.Content), errorText)
			} else {
				turn.finishMessage(process.session.ID, piMessageText(message.Content))
			}
		}
	case "tool_execution_start":
		var toolName string
		_ = json.Unmarshal(event["toolName"], &toolName)
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi tool: " + emptyAsHarness(toolName, "running"), ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "running", Detail: toolName}})
	case "auto_retry_start", "summarization_retry_scheduled":
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi is retrying", ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "reconnecting"}})
	case "compaction_start":
		// Compaction is a context-management phase, never an agent turn: it
		// only surfaces provider-neutral status while a turn already owns the
		// emitter, and an idle manual compact reports through its RPC result.
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi is compacting context", ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "running"}})
	case "compaction_end":
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi finished compacting context", ThreadID: process.session.ID})
	case "agent_settled":
		process.finishTurn(turn)
	}
}

func piMessageText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var result strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			result.WriteString(block.Text)
		}
	}
	return result.String()
}

func (turn *piTurn) startAssistant(threadID string) {
	turn.mu.Lock()
	prefix := ""
	if turn.text.Len() > 0 && !strings.HasSuffix(turn.text.String(), "\n") {
		prefix = "\n"
		turn.text.WriteByte('\n')
	}
	turn.currentMessage.Reset()
	turn.assistantOpen = true
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil && prefix != "" {
		emit(core.Event{Kind: core.EventDelta, Text: prefix, ThreadID: threadID})
	}
}

func (turn *piTurn) appendText(threadID, text string) {
	turn.mu.Lock()
	prefix := ""
	if !turn.assistantOpen && turn.text.Len() > 0 {
		turn.text.WriteByte('\n')
		prefix = "\n"
		turn.currentMessage.Reset()
	}
	turn.assistantOpen = true
	turn.text.WriteString(text)
	turn.currentMessage.WriteString(text)
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil {
		emit(core.Event{Kind: core.EventDelta, Text: prefix + text, ThreadID: threadID})
	}
}

// reconcileMessage merges the provider's authoritative text for the just-ended
// assistant message into the streamed transcript and reports the suffix that
// had not been streamed yet. Callers hold turn.mu.
func (turn *piTurn) reconcileMessage(authoritative string) string {
	current := turn.currentMessage.String()
	missing := ""
	if authoritative != "" && current != authoritative {
		switch {
		case strings.HasPrefix(authoritative, current):
			missing = strings.TrimPrefix(authoritative, current)
		case strings.HasSuffix(current, authoritative):
			// The complete authoritative item was already present in the
			// streamed text, possibly with provider diagnostic prose before it.
		default:
			if current != "" {
				missing = "\n"
			}
			missing += authoritative
		}
		turn.text.WriteString(missing)
		turn.currentMessage.WriteString(missing)
	}
	return missing
}

// finishMessage records one successful assistant message. The completed item
// becomes part of the settled turn and clears any pending error from an
// earlier failed attempt, because Pi retries transient failures inside the same
// settled run and only the retry's message belongs to the result.
func (turn *piTurn) finishMessage(threadID, authoritative string) {
	turn.mu.Lock()
	missing := turn.reconcileMessage(authoritative)
	turn.successfulMessages = append(turn.successfulMessages, turn.currentMessage.String())
	turn.lastMessage = authoritative
	if turn.lastMessage == "" {
		turn.lastMessage = turn.currentMessage.String()
	}
	turn.errorText = ""
	turn.assistantOpen = false
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil && missing != "" {
		emit(core.Event{Kind: core.EventDelta, Text: missing, ThreadID: threadID})
	}
}

// failMessage records a failed assistant message. Pi drops the errored message
// and retries it, so its streamed text is reconciled for live output but never
// becomes part of the settled turn; a later successful message clears the
// pending error.
func (turn *piTurn) failMessage(threadID, authoritative, errorText string) {
	turn.mu.Lock()
	missing := turn.reconcileMessage(authoritative)
	turn.errorText = errorText
	turn.assistantOpen = false
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil && missing != "" {
		emit(core.Event{Kind: core.EventDelta, Text: missing, ThreadID: threadID})
	}
}

func (turn *piTurn) emitEvent(event core.Event) {
	turn.mu.Lock()
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil {
		emit(event)
	}
}

func (process *piProcess) finishTurn(turn *piTurn) {
	turn.deliveryMu.Lock()
	turn.mu.Lock()
	if turn.completed {
		turn.mu.Unlock()
		turn.deliveryMu.Unlock()
		return
	}
	turn.completed = true
	text := turn.text.String()
	last := turn.lastMessage
	errorText := turn.errorText
	successful := len(turn.successfulMessages) > 0
	if successful {
		// Pi retries transient provider failures inside one settled run and
		// drops each errored message. Only successful assistant messages are
		// part of the delivered turn, so a late failure never swallows the
		// answer that the run actually produced.
		text = strings.Join(turn.successfulMessages, "\n")
	} else if last == "" {
		last = text
	}
	emit := turn.emit
	turn.mu.Unlock()
	process.mu.Lock()
	if process.active == turn {
		process.active = nil
	}
	process.mu.Unlock()
	turn.deliveryMu.Unlock()
	if emit == nil {
		return
	}
	if errorText != "" && !successful {
		emit(core.Event{Kind: core.EventError, Text: errorText, ThreadID: process.session.ID, Done: true,
			Execution: &core.ExecutionStatus{State: "error", Detail: errorText}})
		return
	}
	emit(core.Event{Kind: core.EventFinal, Text: text, FinalText: &last, ThreadID: process.session.ID, Done: true,
		Execution: &core.ExecutionStatus{State: "finishing"}})
}

func (process *piProcess) waitLoop() {
	err := process.cmd.Wait()
	if err == nil {
		err = errors.New("Pi RPC process exited")
	}
	process.fail(err)
}

func (process *piProcess) fail(err error) {
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return
	}
	process.closed = true
	pending := process.pending
	process.pending = map[string]chan piResponse{}
	turn := process.active
	process.active = nil
	process.stdin = nil
	cancel := process.cancel
	process.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, waiter := range pending {
		waiter <- piResponse{Error: err}
	}
	if turn != nil {
		turn.emitEvent(core.Event{Kind: core.EventError, Text: err.Error(), ThreadID: process.session.ID, Done: true,
			Execution: &core.ExecutionStatus{State: "error", Detail: err.Error()}})
	}
	if process.owner != nil && process.key != "" {
		process.owner.mu.Lock()
		if process.owner.processes[process.key] == process {
			delete(process.owner.processes, process.key)
		}
		process.owner.mu.Unlock()
	}
}

func (process *piProcess) close() {
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return
	}
	process.closed = true
	stdin := process.stdin
	process.stdin = nil
	cancel := process.cancel
	process.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
}

func (p *Pi) lockForKey(key string) *sync.Mutex {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	lock := p.keyLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		p.keyLocks[key] = lock
	}
	return lock
}

func (p *Pi) sessionPolicyLocked(model, effort string) string {
	cfg := p.config
	cfg.Model = model
	cfg.Effort = effort
	return piSessionPolicy(cfg)
}

func piSessionPolicy(cfg HarnessConfig) string {
	return strings.Join([]string{cfg.Command, cfg.Cwd, cfg.Model, cfg.Effort, cfg.Sandbox}, "\x1f")
}

// piProcessEnvironment applies HarnessConfig.Env overrides over the inherited
// process environment. Later duplicate names win, matching exec's own
// deduplication, so the launched Pi process observes the same effective
// environment that external session resolution assumed.
func piProcessEnvironment(overrides []string) []string {
	if len(overrides) == 0 {
		return nil
	}
	return append(os.Environ(), overrides...)
}

func (p *Pi) loadSessions() error {
	if p.config.SessionsFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.config.SessionsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &p.sessions); err != nil {
		return fmt.Errorf("decode Pi session map: %w", err)
	}
	return nil
}

func (p *Pi) saveSessionsLocked() error {
	if p.config.SessionsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.config.SessionsFile), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p.sessions, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(p.config.SessionsFile, append(data, '\n'), 0o600)
}

func emptyAsHarness(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
