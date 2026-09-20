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
// only reads a bounded first line from each candidate file.
const (
	piCompactMaxInstructions = 4096

	piSettingsMaxBytes          = 1 << 20
	piSessionScanMaxDepth       = 8
	piSessionScanMaxDirectories = 512
	piSessionScanMaxFiles       = 4096
	piSessionHeaderMaxBytes     = 64 << 10
	piSessionSourceMaxBytes     = 256 << 20
	piControlErrorMaxRunes      = 400
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

// CompactSession performs one manual compaction through Pi RPC. It is
// idle-only and resumes an existing persisted session when no process is
// running; it never creates a session and never replaces the stored file.
func (p *Pi) CompactSession(ctx context.Context, key, instructions string) (CompactResult, error) {
	if !utf8.ValidString(instructions) {
		return CompactResult{}, errors.New("Pi compaction instructions must be valid UTF-8")
	}
	if utf8.RuneCountInString(instructions) > piCompactMaxInstructions {
		return CompactResult{}, fmt.Errorf("Pi compaction instructions must be at most %d characters", piCompactMaxInstructions)
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
		return CompactResult{}, fmt.Errorf("Pi rejected the compaction request: %s", piSafeErrorText(err, session.Path, p.sessionDirectory(cfg)))
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

// resumeExistingProcess returns an idle process for one persisted session,
// reusing a policy-matching live process and otherwise resuming the exact
// stored file. Every start is validated before persistence, and an unexpected
// file created by a failed resume is removed only when it is a regular file
// inside the Spynel session directory.
func (p *Pi) resumeExistingProcess(ctx context.Context, key string, session piSession) (*piProcess, error) {
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return nil, errors.New("Pi harness is not running")
	}
	cfg := p.config
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
		piRemoveSessionFile(sessionDir, created, session.Path)
		return nil, fmt.Errorf("resume Pi session: %s", piSafeErrorText(err, session.Path, sessionDir))
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		process.close()
		piRemoveSessionFile(sessionDir, created, session.Path)
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
// never opens or mutates the source file.
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
	source, err := resolveExternalPiSession(sessionID, cfg.Cwd)
	if err != nil {
		return SessionInfo{}, err
	}
	before, err := os.Stat(source.Path)
	if err != nil || !before.Mode().IsRegular() {
		return SessionInfo{}, errors.New("the direct Pi session file is unavailable")
	}
	sessionDir := p.sessionDirectory(cfg)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return SessionInfo{}, err
	}
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
		return nil
	}
	args := []string{"--mode", "rpc", "--session-dir", sessionDir, "--fork", source.Path}
	fork, err := p.startProcessArgs(ctx, key, args, cfg, true, false, validate)
	if err != nil {
		piRemoveSessionFile(sessionDir, created, source.Path)
		return SessionInfo{}, fmt.Errorf("fork Pi session: %s", piSafeErrorText(err, source.Path, sessionDir))
	}
	after, statErr := os.Stat(source.Path)
	if statErr != nil || !piSourceUnchanged(before, after) {
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path)
		return SessionInfo{}, errors.New("the direct Pi session changed while it was being forked; close direct Pi and retry")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path)
		return SessionInfo{}, errors.New("Pi harness closed while importing a session")
	}
	if p.processes[key] != nil || p.sessions[key].ID != "" {
		p.mu.Unlock()
		fork.close()
		piRemoveSessionFile(sessionDir, created, source.Path)
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
		piRemoveSessionFile(sessionDir, created, source.Path)
		return SessionInfo{}, err
	}
	return SessionInfo{ID: fork.session.ID, Path: fork.session.Path, Command: cfg.Command}, nil
}

// resolveExternalPiSession finds exactly one regular JSONL file whose first
// header is a supported Pi session header with the exact requested UUID and a
// canonical cwd equal to the Spynel harness cwd. Zero, ambiguous, unreadable,
// or foreign-workspace matches fail closed without naming other paths.
func resolveExternalPiSession(sessionID, cwd string) (piSessionSource, error) {
	if !piSessionIDPattern.MatchString(sessionID) {
		return piSessionSource{}, errors.New("Pi import requires the full canonical session UUID")
	}
	budget := &piSessionScanBudget{}
	var matches []piSessionSource
	for _, root := range piCandidateSessionRoots(cwd) {
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
// effective process environment, readable global and project settings, and
// the standard Pi agent sessions directory. Relative settings paths resolve
// from the Pi process working directory, which is the Spynel harness cwd.
func piCandidateSessionRoots(cwd string) []string {
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
	agentDir := piEffectiveAgentDir()
	add(piEnvironmentValue("PI_CODING_AGENT_SESSION_DIR"))
	if agentDir != "" {
		add(piSettingsSessionDir(filepath.Join(agentDir, "settings.json"), cwd))
		add(filepath.Join(agentDir, "sessions"))
	}
	add(piSettingsSessionDir(filepath.Join(cwd, ".pi", "settings.json"), cwd))
	return roots
}

func piEffectiveAgentDir() string {
	if value := piEnvironmentValue("PI_CODING_AGENT_DIR"); value != "" {
		home, _ := os.UserHomeDir()
		return piExpandPath(value, home)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

func piEnvironmentValue(name string) string {
	value, ok := os.LookupEnv(name)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
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
// Unreadable candidates fail closed, while directories beyond the depth bound
// are skipped so a deep unrelated tree cannot exhaust the scan.
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
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("Pi session lookup could not read a candidate session directory")
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		if entry.IsDir() {
			if depth+1 >= piSessionScanMaxDepth {
				continue
			}
			if err := walkPiSessionRoot(filepath.Join(root, name), depth+1, sessionID, budget, matches); err != nil {
				return err
			}
			continue
		}
		if err := budget.scanEntry(); err != nil {
			return err
		}
		if !strings.HasSuffix(strings.ToLower(name), ".jsonl") {
			continue
		}
		source, ok, err := readPiSessionHeader(filepath.Join(root, name), sessionID)
		if err != nil {
			return err
		}
		if ok {
			*matches = append(*matches, source)
		}
	}
	return nil
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

// piRemoveSessionFile removes only a regular file inside the Spynel session
// directory that is not the validated source. It is the cleanup boundary for
// failed control operations, never a rename or mutation of a source session.
func piRemoveSessionFile(sessionDir, path, source string) {
	if path == "" || source == "" {
		return
	}
	if samePiPath(path, source) || !piPathWithin(sessionDir, path) {
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

// piSafeErrorText bounds and sanitizes one provider error for a user-facing
// control result. Known session paths are redacted and control characters are
// removed so unrelated session content never reaches a command response.
func piSafeErrorText(err error, sensitive ...string) string {
	text := err.Error()
	for _, value := range sensitive {
		if value != "" {
			text = strings.ReplaceAll(text, value, "<session>")
		}
	}
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > piControlErrorMaxRunes {
		text = string(runes[:piControlErrorMaxRunes]) + "..."
	}
	return text
}
