package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Registry is the control plane for dynamic sources and handlers. It compiles
// JSON specs into live Subscriptions (registered on the Bus) and running Sources
// (poller goroutines), persists the specs, and rebuilds them on boot so a
// long-running cma-service keeps its configured event wiring across restarts.
//
// One file holds all specs: <dir>/eventbus/_registry.json.
type Registry struct {
	bus  *Bus
	path string

	mu       sync.Mutex
	sources  map[string]SourceSpec
	handlers map[string]HandlerSpec
	cancels  map[string]context.CancelFunc // per running source
	ctx      context.Context               // parent for source goroutines
}

type registryState struct {
	Sources  map[string]SourceSpec  `json:"sources"`
	Handlers map[string]HandlerSpec `json:"handlers"`
}

// NewRegistry builds the registry over bus, loads any persisted specs, and
// rebuilds them: handlers are registered first, then sources start (so the first
// poll dispatches to live handlers). ctx bounds every source goroutine.
func NewRegistry(ctx context.Context, bus *Bus, dir string) (*Registry, error) {
	r := &Registry{
		bus:      bus,
		path:     filepath.Join(dir, "eventbus", "_registry.json"),
		sources:  map[string]SourceSpec{},
		handlers: map[string]HandlerSpec{},
		cancels:  map[string]context.CancelFunc{},
		ctx:      ctx,
	}
	st, err := r.load()
	if err != nil {
		return nil, err
	}
	for name, spec := range st.Handlers {
		sub, err := spec.compileToSubscription()
		if err != nil {
			return nil, fmt.Errorf("rebuild handler %q: %w", name, err)
		}
		if err := r.bus.Register(sub); err != nil {
			return nil, fmt.Errorf("rebuild handler %q: %w", name, err)
		}
		r.handlers[name] = spec
	}
	for name, spec := range st.Sources {
		if err := r.startSource(spec); err != nil {
			return nil, fmt.Errorf("rebuild source %q: %w", name, err)
		}
		r.sources[name] = spec
	}
	return r, nil
}

// Bus exposes the underlying bus (e.g. to mount its webhook).
func (r *Registry) Bus() *Bus { return r.bus }

// ----- handlers -----

// AddHandler compiles and registers a handler, then persists it. A duplicate
// name or a bad spec is an error and changes nothing.
func (r *Registry) AddHandler(spec HandlerSpec) error {
	sub, err := spec.compileToSubscription()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[spec.Name]; ok {
		return fmt.Errorf("handler %q already exists", spec.Name)
	}
	if err := r.bus.Register(sub); err != nil {
		return err
	}
	r.handlers[spec.Name] = spec
	return r.persistLocked()
}

// RemoveHandler unregisters and forgets a handler. Returns false if not found.
func (r *Registry) RemoveHandler(name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[name]; !ok {
		return false, nil
	}
	r.bus.Unregister(name)
	delete(r.handlers, name)
	return true, r.persistLocked()
}

// ListHandlers returns the current handler specs.
func (r *Registry) ListHandlers() []HandlerSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]HandlerSpec, 0, len(r.handlers))
	for _, h := range r.handlers {
		out = append(out, h)
	}
	return out
}

// ----- sources -----

// AddSource validates, starts, and persists a source. A duplicate name or
// unknown type is an error and changes nothing.
func (r *Registry) AddSource(spec SourceSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if spec.Name == "" {
		return fmt.Errorf("source name is required")
	}
	if _, ok := r.sources[spec.Name]; ok {
		return fmt.Errorf("source %q already exists", spec.Name)
	}
	if err := r.startSource(spec); err != nil {
		return err
	}
	r.sources[spec.Name] = spec
	return r.persistLocked()
}

// RemoveSource stops and forgets a source. Returns false if not found.
func (r *Registry) RemoveSource(name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[name]; !ok {
		return false, nil
	}
	if cancel := r.cancels[name]; cancel != nil {
		cancel()
		delete(r.cancels, name)
	}
	delete(r.sources, name)
	return true, r.persistLocked()
}

// ListSources returns the current source specs.
func (r *Registry) ListSources() []SourceSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SourceSpec, 0, len(r.sources))
	for _, s := range r.sources {
		out = append(out, s)
	}
	return out
}

// startSource builds the source's fetch and launches its poller goroutine. Must
// be called with r.mu held.
func (r *Registry) startSource(spec SourceSpec) error {
	fetch, err := buildFetch(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	poller := NewPoller(spec.Name, parseInterval(spec.Interval), r.bus, fetch)
	r.cancels[spec.Name] = cancel
	go func() { _ = poller.Run(ctx) }()
	return nil
}

// buildFetch maps a typed SourceSpec to a FetchFunc.
func buildFetch(spec SourceSpec) (FetchFunc, error) {
	switch spec.Type {
	case "codehub-pr":
		if spec.Project == "" {
			return nil, fmt.Errorf("codehub-pr source requires project")
		}
		allow := map[int]bool{}
		for _, iid := range spec.AllowIIDs {
			allow[iid] = true
		}
		src := CodeHubPRSource{
			Bin:         spec.Bin,
			Project:     spec.Project,
			Reviewer:    spec.Reviewer,
			Author:      spec.Author,
			State:       spec.State,
			EventType:   spec.EventType,
			AllowIIDs:   allow,
			MergeStatus: spec.MergeStatus,
		}
		return src.Fetch, nil
	case "workitem":
		if spec.Space == "" && spec.Project == "" {
			return nil, fmt.Errorf("workitem source requires space or project")
		}
		src := WorkItemSource{
			Bin:          spec.Bin,
			Space:        spec.Space,
			Project:      spec.Project,
			Scope:        spec.Scope,
			Belong:       spec.Belong,
			StatusList:   spec.StatusList,
			PageSize:     spec.PageSize,
			EventType:    spec.EventType,
			IDField:      spec.IDField,
			VersionField: spec.VersionField,
		}
		return src.Fetch, nil
	case "github":
		if spec.Repo == "" {
			return nil, fmt.Errorf("github source requires repo (owner/name)")
		}
		token, err := readGitHubToken(spec.TokenFile)
		if err != nil {
			return nil, err
		}
		allow := map[int]bool{}
		for _, n := range spec.AllowNumbers {
			allow[n] = true
		}
		src := &GitHubSource{
			Repo:         spec.Repo,
			Token:        token,
			APIBase:      spec.APIBase,
			State:        spec.State,
			Kinds:        spec.Kinds,
			IssueType:    spec.IssueEventType,
			PRType:       spec.PREventType,
			BotMarker:    spec.BotMarker,
			AllowNumbers: allow,
			since:        time.Now(), // don't replay history on (re)start; act on new activity only
		}
		return src.Fetch, nil
	default:
		return nil, fmt.Errorf("unknown source type %q", spec.Type)
	}
}

// readGitHubToken loads the GitHub PAT the source authenticates with, keeping the
// secret out of the persisted SourceSpec. Resolution: the spec's token_file (a
// path to a file holding the PAT) → GITHUB_TOKEN → GH_TOKEN. A token is required:
// unauthenticated polling is rate-capped at 60/hr AND the self-trigger guard
// needs GET /user to learn the bot login.
func readGitHubToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("github token_file %q: %w", tokenFile, err)
		}
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
		return "", fmt.Errorf("github token_file %q is empty", tokenFile)
	}
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if tok := strings.TrimSpace(os.Getenv(env)); tok != "" {
			return tok, nil
		}
	}
	return "", fmt.Errorf("github source requires a token: set token_file in the source spec, or GITHUB_TOKEN/GH_TOKEN in the environment")
}

// ----- persistence -----

func (r *Registry) load() (registryState, error) {
	st := registryState{Sources: map[string]SourceSpec{}, Handlers: map[string]HandlerSpec{}}
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Sources == nil {
		st.Sources = map[string]SourceSpec{}
	}
	if st.Handlers == nil {
		st.Handlers = map[string]HandlerSpec{}
	}
	return st, nil
}

// persistLocked writes the current specs atomically. Must hold r.mu.
func (r *Registry) persistLocked() error {
	st := registryState{Sources: r.sources, Handlers: r.handlers}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
