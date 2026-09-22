package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Explicit bounds for read-only external session discovery. The walk never
// follows symlinks, never opens more than the bounded candidate count, and
// reads directories incrementally so one huge directory cannot allocate
// unbounded memory before an entry budget is enforced.
const (
	piDirectoryBatchSize             = 256
	piSettingsMaxBytes               = 1 << 20
	piSessionScanMaxDepth            = 8
	piSessionScanMaxDirectories      = 512
	piSessionScanMaxDirectoryEntries = 2048
	piSessionScanMaxFiles            = 4096
	piSessionHeaderMaxBytes          = 64 << 10
	piSessionSourceMaxBytes          = 256 << 20
)

var piSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// piSessionSource is one validated external Pi session file found by exact ID.
type piSessionSource struct {
	ID   string
	Path string
	Cwd  string
}

// SessionInfo reports the current Pi session identity and configured command
// without starting a provider process. A missing conversation session is not
// an error; it returns ok=false so callers can explain how it is created.
func (p *Pi) SessionInfo(key string) (SessionInfo, bool, error) {
	p.mu.Lock()
	session, ok := p.sessions[key]
	command := p.config.Command
	p.mu.Unlock()
	if !ok || session.ID == "" || session.Path == "" {
		return SessionInfo{}, false, nil
	}
	return SessionInfo{ID: session.ID, Path: session.Path, Command: command}, true, nil
}

// SetSessionName assigns one display name to an existing conversation
// session through Pi RPC. It runs under the conversation's per-key lock,
// reuses a matching live process even during an active turn, and otherwise
// resumes the exact stored session, so naming never creates or rotates
// provider state. The returned name is the effective provider value read
// back from get_state after the call; onlyIfEmpty preserves and returns an
// existing provider name without issuing a rename request.
func (p *Pi) SetSessionName(ctx context.Context, key, expectedSessionID, name string, onlyIfEmpty bool) (SessionNameResult, error) {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return SessionNameResult{}, errors.New("Pi session name must be nonempty valid UTF-8")
	}
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return SessionNameResult{}, errors.New("Pi harness is not running")
	}
	cfg := p.config
	session := p.sessions[key]
	process := p.processes[key]
	p.mu.Unlock()
	if session.ID == "" || session.Path == "" {
		return SessionNameResult{}, errors.New("no Pi session exists yet; the first ordinary prompt creates one")
	}
	if expectedSessionID == "" || session.ID != expectedSessionID {
		return SessionNameResult{}, errors.New("the current Pi session does not match the expected session")
	}
	if info, err := os.Stat(session.Path); err != nil || !info.Mode().IsRegular() {
		return SessionNameResult{}, errors.New("the stored Pi session file is unavailable")
	}
	// A live process that already serves the expected session is reused even
	// while its turn is active: naming is metadata-only and must never
	// interrupt provider work. Session identity is authoritative here; a
	// concurrently committed model or effort change cannot redirect the exact
	// session the caller named.
	if !piProcessServesSession(process, expectedSessionID) {
		resumed, err := p.resumeExistingProcess(ctx, key, session)
		if err != nil {
			return SessionNameResult{}, err
		}
		process = resumed
	}
	state, err := p.readPiState(ctx, process, session, cfg)
	if err != nil {
		return SessionNameResult{}, err
	}
	if state.SessionID != expectedSessionID {
		return SessionNameResult{}, errors.New("the current Pi session does not match the expected session")
	}
	if onlyIfEmpty && strings.TrimSpace(state.SessionName) != "" {
		return SessionNameResult{Name: state.SessionName, Changed: false}, nil
	}
	sensitive := append([]string{name, session.Path}, p.piControlSensitivePaths(cfg)...)
	if _, err := process.call(ctx, map[string]any{"type": "set_session_name", "name": name}, nil); err != nil {
		return SessionNameResult{}, fmt.Errorf("Pi rejected the session name: %w", piSafeControlError(err, sensitive...))
	}
	after, err := p.readPiState(ctx, process, session, cfg)
	if err != nil {
		return SessionNameResult{}, err
	}
	if after.SessionID != expectedSessionID {
		return SessionNameResult{}, errors.New("the current Pi session does not match the expected session")
	}
	return SessionNameResult{Name: after.SessionName, Changed: after.SessionName != state.SessionName}, nil
}

// piProcessServesSession reports whether one cached live process already
// serves exactly the expected provider session.
func piProcessServesSession(process *piProcess, expectedSessionID string) bool {
	if process == nil {
		return false
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	return !process.closed && process.session.ID == expectedSessionID
}

// readPiState performs one bounded RPC get_state through an existing process
// and validates that it reported a session identity.
func (p *Pi) readPiState(ctx context.Context, process *piProcess, session piSession, cfg HarnessConfig) (piState, error) {
	data, err := process.call(ctx, map[string]any{"type": "get_state"}, nil)
	if err != nil {
		return piState{}, fmt.Errorf("read Pi session state: %w", piSafeControlError(err, append([]string{session.Path}, p.piControlSensitivePaths(cfg)...)...))
	}
	var state piState
	if err := json.Unmarshal(data, &state); err != nil || state.SessionID == "" {
		return piState{}, errors.New("Pi returned an incompatible get_state result")
	}
	return state, nil
}

// CompactSession performs one manual compaction through Pi RPC. It is
// idle-only and resumes an existing persisted session when no process is
// running; it never creates a session and never replaces the stored file.
func (p *Pi) CompactSession(ctx context.Context, key, instructions string) (CompactResult, error) {
	if !utf8.ValidString(instructions) {
		return CompactResult{}, errors.New("Pi compaction instructions must be valid UTF-8")
	}
	if utf8.RuneCountInString(instructions) > SessionCompactMaxInstructions {
		return CompactResult{}, fmt.Errorf("Pi compaction instructions must be at most %d characters", SessionCompactMaxInstructions)
	}
	instructions = strings.TrimSpace(instructions)
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return CompactResult{}, errors.New("Pi harness is not running")
	}
	cfg := p.config
	session := p.sessions[key]
	p.mu.Unlock()
	if session.ID == "" || session.Path == "" {
		return CompactResult{}, errors.New("no Pi session exists yet; the first ordinary prompt creates one")
	}
	if info, err := os.Stat(session.Path); err != nil || !info.Mode().IsRegular() {
		return CompactResult{}, errors.New("the stored Pi session file is unavailable")
	}
	process, err := p.resumeExistingProcess(ctx, key, session)
	if err != nil {
		return CompactResult{}, err
	}
	process.mu.Lock()
	active := process.active != nil
	process.mu.Unlock()
	if active {
		return CompactResult{}, errors.New("cannot compact a Pi session while its turn is active")
	}
	request := map[string]any{"type": "compact"}
	if instructions != "" {
		request["customInstructions"] = instructions
	}
	data, err := process.call(ctx, request, nil)
	if err != nil {
		return CompactResult{}, fmt.Errorf("Pi rejected the compaction request: %w", piSafeControlError(err, append([]string{session.Path}, p.piControlSensitivePaths(cfg)...)...))
	}
	var response struct {
		TokensBefore         *int `json:"tokensBefore"`
		EstimatedTokensAfter *int `json:"estimatedTokensAfter"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.TokensBefore == nil {
		return CompactResult{}, errors.New("Pi returned an incompatible compact result")
	}
	result := CompactResult{TokensBefore: max(0, *response.TokensBefore)}
	if response.EstimatedTokensAfter != nil {
		result.TokensAfterKnown = true
		result.TokensAfter = max(0, *response.EstimatedTokensAfter)
	}
	return result, nil
}

// resumeExistingProcess returns an idle process for one persisted session. It
// refuses a stored session whose policy was captured under a different
// command, working directory, model, effort, or sandbox so a control
// operation can never resume and re-persist a session under the wrong
// configuration; the caller must send an ordinary prompt to create the
// active-policy session instead. A policy-matching live process is reused and
// otherwise the exact stored file is resumed. Every start is validated before
// persistence, and an unexpected file created by a failed resume is removed
// only when it is a regular file inside the Spynel session directory.
func (p *Pi) resumeExistingProcess(ctx context.Context, key string, session piSession) (*piProcess, error) {
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return nil, errors.New("Pi harness is not running")
	}
	cfg := p.config
	if session.Policy != piSessionPolicy(cfg) {
		p.mu.Unlock()
		return nil, errors.New("the stored Pi session was created with a different harness configuration; send an ordinary prompt to create a session under the current configuration")
	}
	if process := p.processes[key]; process != nil {
		process.mu.Lock()
		active := process.active != nil
		policyMatches := process.session.Policy == piSessionPolicy(cfg)
		matchesSession := process.session.ID == session.ID
		process.mu.Unlock()
		if active {
			p.mu.Unlock()
			return nil, errors.New("cannot change a Pi session while its turn is active")
		}
		if policyMatches && matchesSession {
			p.mu.Unlock()
			return process, nil
		}
		delete(p.processes, key)
		p.mu.Unlock()
		process.close()
		p.mu.Lock()
	}
	p.mu.Unlock()
	sessionDir := p.sessionDirectory(cfg)
	// Snapshot the directory before resume so failure cleanup can only remove a
	// file the snapshot proves was absent; a pre-existing sibling reported by a
	// hostile or confused provider survives untouched.
	existing := snapshotPiSessionDirectory(sessionDir)
	created := ""
	validate := func(state piState) error {
		created = state.SessionFile
		if state.SessionID != session.ID {
			return errors.New("Pi did not resume the stored session")
		}
		if !samePiPath(state.SessionFile, session.Path) {
			return errors.New("Pi resumed a different session file")
		}
		return nil
	}
	process, err := p.startProcessValidated(ctx, key, session, false, cfg.Model, cfg.Effort, validate)
	if err != nil {
		piRemoveSessionFile(sessionDir, created, session.Path, existing)
		return nil, fmt.Errorf("resume Pi session: %w", piSafeControlError(err, append([]string{session.Path}, p.piControlSensitivePaths(cfg)...)...))
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		process.close()
		piRemoveSessionFile(sessionDir, created, session.Path, existing)
		return nil, errors.New("Pi harness closed while resuming a session")
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

// ImportSession resolves one canonical external Pi session UUID without
// starting Pi, then forks the exact validated file into the workspace-local
// session directory through a real Pi RPC process. It refuses an existing
// conversation session, validates the fork before persisting anything, and
// never opens or mutates the source file. Cleanup can only remove a reported
// fork file the pre-import snapshot proves was absent; if Pi creates a fork
// file and fails before get_state ever reports its path, that file is an
// unavoidable inert orphan: the adapter has no path to remove, and scanning
// the shared session directory to guess one could delete another
// conversation's session. The session map stays unchanged in that case, so
// the orphan is never resumed or referenced.
func (p *Pi) ImportSession(ctx context.Context, key, sessionID string) (SessionInfo, error) {
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return SessionInfo{}, errors.New("Pi harness is not running")
	}
	cfg := p.config
	stored := p.sessions[key]
	process := p.processes[key]
	p.mu.Unlock()
	if stored.ID != "" {
		return SessionInfo{}, errors.New("this conversation already has a harness session; use /clear before importing another")
	}
	if process != nil {
		process.mu.Lock()
		active := process.active != nil
		process.mu.Unlock()
		if active {
			return SessionInfo{}, errors.New("cannot import a Pi session while a turn is active")
		}
	}
	source, err := resolveExternalPiSession(sessionID, cfg.Cwd, cfg.Env)
	if err != nil {
		return SessionInfo{}, err
	}
	before, err := os.Stat(source.Path)
	if err != nil || !before.Mode().IsRegular() {
		return SessionInfo{}, errors.New("the direct Pi session file is unavailable")
	}
	sessionDir := p.sessionDirectory(cfg)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return SessionInfo{}, fmt.Errorf("prepare the Spynel Pi session directory: %w", piSafeControlError(err, sessionDir))
	}
	// Snapshot the session directory before forking. Cleanup may remove only a
	// reported path the snapshot proves was absent, so Pi can never trick
	// import recovery into deleting a pre-existing sibling session. If the
	// snapshot cannot prove absence (unreadable or over budget), cleanup fails
	// closed and leaves the path in place.
	existing := snapshotPiSessionDirectory(sessionDir)
	created := ""
	validate := func(state piState) error {
		created = state.SessionFile
		if state.SessionID == "" || state.SessionFile == "" {
			return errors.New("Pi returned an incompatible fork state")
		}
		if state.SessionID == source.ID || samePiPath(state.SessionFile, source.Path) {
			return errors.New("Pi did not create a distinct forked session")
		}
		if !piPathWithin(sessionDir, state.SessionFile) {
			return errors.New("Pi stored the forked session outside the Spynel session directory")
		}
		if !existing.absentBefore(state.SessionFile) {
			return errors.New("Pi reported a session file that already existed before this import; import fails closed")
		}
		return nil
	}
	args := []string{"--mode", "rpc", "--session-dir", sessionDir, "--fork", source.Path}
	fork, err := p.startProcessArgs(ctx, key, args, cfg, true, false, validate)
	if err != nil {
		piRemoveSessionFile(sessionDir, created, source.Path, existing)
		return SessionInfo{}, fmt.Errorf("fork Pi session: %w", piSafeControlError(err, append([]string{source.Path}, p.piControlSensitivePaths(cfg)...)...))
	}
	after, statErr := os.Stat(source.Path)
	if statErr != nil || !piSourceUnchanged(before, after) {
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path, existing)
		return SessionInfo{}, errors.New("the direct Pi session changed while it was being forked; close direct Pi and retry")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path, existing)
		return SessionInfo{}, errors.New("Pi harness closed while importing a session")
	}
	if p.processes[key] != nil || p.sessions[key].ID != "" {
		p.mu.Unlock()
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path, existing)
		return SessionInfo{}, errors.New("this conversation already has a harness session; use /clear before importing another")
	}
	p.processes[key] = fork
	p.sessions[key] = fork.session
	err = p.saveSessionsLocked()
	p.mu.Unlock()
	if err != nil {
		p.mu.Lock()
		if p.processes[key] == fork {
			delete(p.processes, key)
		}
		if p.sessions[key] == fork.session {
			delete(p.sessions, key)
		}
		p.mu.Unlock()
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path, existing)
		return SessionInfo{}, fmt.Errorf("persist the imported Pi session: %w", piSafeControlError(err, append([]string{source.Path}, p.piControlSensitivePaths(cfg)...)...))
	}
	return SessionInfo{ID: fork.session.ID, Path: fork.session.Path, Command: cfg.Command}, nil
}

// resolveExternalPiSession finds exactly one regular JSONL file whose first
// header is a supported Pi session header with the exact requested UUID and a
// canonical cwd equal to the Spynel harness cwd. Zero, ambiguous, unreadable,
// or foreign-workspace matches fail closed without naming other paths.
func resolveExternalPiSession(sessionID, cwd string, env []string) (piSessionSource, error) {
	if !piSessionIDPattern.MatchString(sessionID) {
		return piSessionSource{}, errors.New("Pi import requires the full canonical session UUID")
	}
	budget := &piSessionScanBudget{}
	var matches []piSessionSource
	for _, root := range piCandidateSessionRoots(cwd, piEffectiveEnvLookup(env)) {
		if err := walkPiSessionRoot(root, 0, sessionID, budget, &matches); err != nil {
			return piSessionSource{}, err
		}
		if len(matches) > 1 {
			return piSessionSource{}, errors.New("multiple direct Pi sessions match that ID; import fails closed")
		}
	}
	if len(matches) == 0 {
		return piSessionSource{}, errors.New("no direct Pi session with that ID was found")
	}
	source := matches[0]
	if !samePiPath(source.Cwd, cwd) {
		return piSessionSource{}, errors.New("that Pi session belongs to a different workspace; import fails closed")
	}
	return source, nil
}

// piCandidateSessionRoots derives bounded read-only lookup roots from the
// effective Pi process environment, readable settings, and the standard Pi
// agent sessions directory. Relative values resolve from the Pi process
// working directory, which is the Spynel harness cwd, matching Pi's own
// SessionManager and FileSettingsStorage resolution. Project settings
// override global settings exactly like Pi's settings merge.
func piCandidateSessionRoots(cwd string, lookup func(string) string) []string {
	roots := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(value string) {
		value = piExpandPath(value, cwd)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		roots = append(roots, value)
	}
	agentDir := piEffectiveAgentDir(cwd, lookup)
	projectSettingsDir := piSettingsSessionDir(filepath.Join(cwd, ".pi", "settings.json"), cwd)
	globalSettingsDir := ""
	if agentDir != "" {
		globalSettingsDir = piSettingsSessionDir(filepath.Join(agentDir, "settings.json"), cwd)
	}
	add(lookup("PI_CODING_AGENT_SESSION_DIR"))
	if projectSettingsDir != "" {
		add(projectSettingsDir)
	} else {
		add(globalSettingsDir)
	}
	if agentDir != "" {
		add(filepath.Join(agentDir, "sessions"))
	}
	return roots
}

// piEffectiveAgentDir resolves Pi's agent config directory from the effective
// environment. Pi tilde-expands PI_CODING_AGENT_DIR and resolves a relative
// value from the process working directory, so the same rule applies here.
func piEffectiveAgentDir(cwd string, lookup func(string) string) string {
	if value := lookup("PI_CODING_AGENT_DIR"); value != "" {
		return piExpandPath(value, cwd)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// piEffectiveEnvLookup resolves environment values from the inherited process
// environment with HarnessConfig.Env overrides applied last, so a later
// duplicate name wins exactly like process launch. This keeps external
// session resolution and the launched provider on the same effective
// environment.
func piEffectiveEnvLookup(overrides []string) func(string) string {
	values := make(map[string]string, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	for _, entry := range overrides {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	return func(name string) string {
		return strings.TrimSpace(values[name])
	}
}

// piSettingsSessionDir reads only the bounded sessionDir value from one Pi
// settings file. Missing, oversized, malformed, or empty input yields no root.
func piSettingsSessionDir(path, cwd string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > piSettingsMaxBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var settings struct {
		SessionDir string `json:"sessionDir"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ""
	}
	return piExpandPath(settings.SessionDir, cwd)
}

// piExpandPath expands one leading tilde and resolves a relative path from the
// provided working directory, mirroring Pi's session directory resolution.
func piExpandPath(value, cwd string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, value[2:])
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(cwd, value)
	}
	return filepath.Clean(value)
}

// piSessionScanBudget enforces explicit directory and candidate-file bounds
// for one read-only lookup.
type piSessionScanBudget struct {
	directories int
	files       int
}

var errPiSessionScanLimit = errors.New("Pi session lookup exceeded its bounded scan limits")

func (b *piSessionScanBudget) enterDirectory() error {
	b.directories++
	if b.directories > piSessionScanMaxDirectories {
		return errPiSessionScanLimit
	}
	return nil
}

func (b *piSessionScanBudget) scanEntry() error {
	b.files++
	if b.files > piSessionScanMaxFiles {
		return errPiSessionScanLimit
	}
	return nil
}

// walkPiSessionRoot descends one candidate root without following symlinks.
// Directory entries stream through bounded batches with a per-directory entry
// cap, so an enormous directory cannot allocate unbounded memory before the
// explicit budget is enforced. Unreadable candidates fail closed, while
// directories beyond the depth bound are skipped so a deep unrelated tree
// cannot exhaust the scan.
func walkPiSessionRoot(root string, depth int, sessionID string, budget *piSessionScanBudget, matches *[]piSessionSource) error {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.New("Pi session lookup could not read a candidate session directory")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil
	}
	if err := budget.enterDirectory(); err != nil {
		return err
	}
	return readPiDirectoryEntries(root, piSessionScanMaxDirectoryEntries, func(entry os.DirEntry) error {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if depth+1 >= piSessionScanMaxDepth {
				return nil
			}
			return walkPiSessionRoot(filepath.Join(root, name), depth+1, sessionID, budget, matches)
		}
		if err := budget.scanEntry(); err != nil {
			return err
		}
		if !strings.HasSuffix(strings.ToLower(name), ".jsonl") {
			return nil
		}
		source, ok, err := readPiSessionHeader(filepath.Join(root, name), sessionID)
		if err != nil {
			return err
		}
		if ok {
			*matches = append(*matches, source)
		}
		return nil
	})
}

// readPiDirectoryEntries streams one directory in small batches so a huge
// directory never allocates its complete entry list before the caller's
// per-directory cap is applied. Reading stops with errPiSessionScanLimit once
// the cap is exceeded.
func readPiDirectoryEntries(dir string, limit int, visit func(entry os.DirEntry) error) error {
	file, err := os.Open(dir)
	if err != nil {
		return errors.New("Pi session lookup could not read a candidate session directory")
	}
	defer file.Close()
	count := 0
	for {
		entries, readErr := file.ReadDir(piDirectoryBatchSize)
		for _, entry := range entries {
			count++
			if count > limit {
				return errPiSessionScanLimit
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return errors.New("Pi session lookup could not read a candidate session directory")
		}
	}
}

// readPiSessionHeader validates one candidate file from its bounded first
// line. A regular file whose header is a supported Pi session with the exact
// ID is a match; anything else is skipped rather than reported, so unrelated
// files never influence or leak into the result.
func readPiSessionHeader(path, sessionID string) (piSessionSource, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return piSessionSource{}, false, errors.New("Pi session lookup could not read a candidate session file")
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > piSessionSourceMaxBytes {
		return piSessionSource{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return piSessionSource{}, false, errors.New("Pi session lookup could not read a candidate session file")
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, piSessionHeaderMaxBytes+1), piSessionHeaderMaxBytes+1)
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return piSessionSource{}, false, errors.New("Pi session lookup could not read a candidate session file")
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 || len(line) > piSessionHeaderMaxBytes {
		return piSessionSource{}, false, nil
	}
	var header struct {
		Type    string `json:"type"`
		Version *int   `json:"version"`
		ID      string `json:"id"`
		Cwd     string `json:"cwd"`
	}
	if json.Unmarshal(line, &header) != nil {
		return piSessionSource{}, false, nil
	}
	if header.Type != "session" || header.ID != sessionID {
		return piSessionSource{}, false, nil
	}
	if header.Version != nil && (*header.Version < 1 || *header.Version > 3) {
		return piSessionSource{}, false, nil
	}
	if strings.TrimSpace(header.Cwd) == "" {
		return piSessionSource{}, false, nil
	}
	return piSessionSource{ID: header.ID, Path: path, Cwd: header.Cwd}, true, nil
}

// samePiPath compares two cleaned paths, falling back to their resolved form
// when both currently resolve so a symlinked workspace spelling still matches.
func samePiPath(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(resolvedLeft) == filepath.Clean(resolvedRight)
}

// piPathWithin reports whether path is dir itself or a descendant of dir.
func piPathWithin(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// piSessionSnapshot records the subtree that existed in one Spynel session
// directory before a control operation. Cleanup removes a reported path only
// when the snapshot proves it was absent beforehand, so a hostile or confused
// provider can never trick import or resume recovery into deleting a
// pre-existing sibling session. An unreadable or over-budget snapshot yields
// known=false and makes cleanup fail closed.
type piSessionSnapshot struct {
	dir   string
	known bool
	paths map[string]struct{}
}

func (s piSessionSnapshot) absentBefore(path string) bool {
	if !s.known || path == "" || s.dir == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(s.dir), filepath.Clean(path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	_, existed := s.paths[relative]
	return !existed
}

// snapshotPiSessionDirectory captures the pre-existing subtree with the same
// directory, entry, and depth budgets as session discovery.
func snapshotPiSessionDirectory(dir string) piSessionSnapshot {
	snapshot := piSessionSnapshot{dir: filepath.Clean(dir), known: true, paths: map[string]struct{}{}}
	if err := snapshotPiSessionDirectoryInto(dir, "", 0, &snapshot); err != nil {
		snapshot.known = false
		snapshot.paths = nil
	}
	return snapshot
}

func snapshotPiSessionDirectoryInto(dir, prefix string, depth int, snapshot *piSessionSnapshot) error {
	if depth > piSessionScanMaxDepth {
		return errPiSessionScanLimit
	}
	return readPiDirectoryEntries(dir, piSessionScanMaxDirectoryEntries, func(entry os.DirEntry) error {
		if len(snapshot.paths) >= piSessionScanMaxFiles {
			return errPiSessionScanLimit
		}
		relative := entry.Name()
		if prefix != "" {
			relative = filepath.Join(prefix, entry.Name())
		}
		snapshot.paths[relative] = struct{}{}
		if entry.Type()&os.ModeSymlink == 0 && entry.IsDir() {
			return snapshotPiSessionDirectoryInto(filepath.Join(dir, entry.Name()), relative, depth+1, snapshot)
		}
		return nil
	})
}

// piRemoveSessionFile removes only a regular file inside the Spynel session
// directory that is not the validated source and that the pre-operation
// snapshot proves was absent. It is the cleanup boundary for failed control
// operations, never a rename or mutation of a source or pre-existing session.
func piRemoveSessionFile(sessionDir, path, source string, snapshot piSessionSnapshot) {
	if path == "" || source == "" {
		return
	}
	if samePiPath(path, source) || !piPathWithin(sessionDir, path) || !snapshot.absentBefore(path) {
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	_ = os.Remove(path)
}

// piSourceUnchanged reports whether one source file had the same identity,
// size, and modification time across a fork operation.
func piSourceUnchanged(before, after os.FileInfo) bool {
	if before == nil || after == nil {
		return false
	}
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

// piControlError preserves the original cause for errors.Is while presenting
// a bounded, sanitized message.
type piControlError struct {
	message string
	cause   error
}

func (e *piControlError) Error() string { return e.message }
func (e *piControlError) Unwrap() error { return e.cause }

// piSafeControlError sanitizes one control-operation error so session paths
// and other sensitive values never leave the adapter toward a user reply.
func piSafeControlError(err error, sensitive ...string) error {
	if err == nil {
		return nil
	}
	return &piControlError{message: SafeControlErrorText(err, sensitive...), cause: err}
}

// piControlSensitivePaths lists adapter-owned locations that must never
// appear in a user-facing control error.
func (p *Pi) piControlSensitivePaths(cfg HarnessConfig) []string {
	values := []string{cfg.Cwd, cfg.SessionsFile, p.sessionDirectory(cfg)}
	if cfg.SessionsFile != "" {
		values = append(values, filepath.Dir(cfg.SessionsFile))
	}
	return values
}
