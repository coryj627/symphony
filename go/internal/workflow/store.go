package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"go.yaml.in/yaml/v3"
)

var ErrStoreClosed = errors.New("workflow_store_closed")

type ProviderValidator func(EffectiveConfig) []FieldError

// Store is the single owner of the last-known-good workflow snapshot.
type Store interface {
	Current() (Snapshot, bool)
	Load(context.Context) (Snapshot, error)
	Validate(context.Context, []byte) ValidationResult
	Save(context.Context, SaveCommand) (Snapshot, error)
	Changes() <-chan Change
}

type Change struct {
	Snapshot   Snapshot
	Digest     string
	Validation ValidationResult
}

type FileStore struct {
	path             string
	pathMu           *pathGate
	lookup           LookupEnv
	providerValidate ProviderValidator
	atomic           atomicOperations

	stateMu  sync.RWMutex
	current  Snapshot
	hasValue bool

	changes   chan Change
	publishMu sync.Mutex

	lifecycleMu sync.Mutex
	closing     bool
	active      sync.WaitGroup
	activeCount int
	gateRelease sync.Once
	closeOnce   sync.Once
	closeErr    error
	closed      chan struct{}
	stopping    chan struct{}

	watcher *fsnotify.Watcher
	cancel  context.CancelFunc
	watchWG sync.WaitGroup
}

type pathGate struct {
	token chan struct{}
	refs  int
}

func newPathGate() *pathGate {
	gate := &pathGate{token: make(chan struct{}, 1)}
	gate.token <- struct{}{}
	return gate
}
func (gate *pathGate) acquire(ctx context.Context, stopping <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stopping:
		return ErrStoreClosed
	case <-gate.token:
	}
	select {
	case <-ctx.Done():
		gate.release()
		return ctx.Err()
	case <-stopping:
		gate.release()
		return ErrStoreClosed
	default:
		return nil
	}
}
func (gate *pathGate) release() { gate.token <- struct{}{} }

var workflowPathTransactions = struct {
	sync.Mutex
	locks map[string]*pathGate
}{locks: make(map[string]*pathGate)}

func retainPathTransaction(path string) *pathGate {
	workflowPathTransactions.Lock()
	defer workflowPathTransactions.Unlock()
	if workflowPathTransactions.locks[path] == nil {
		workflowPathTransactions.locks[path] = newPathGate()
	}
	gate := workflowPathTransactions.locks[path]
	gate.refs++
	return gate
}
func releasePathTransaction(path string, gate *pathGate) {
	workflowPathTransactions.Lock()
	defer workflowPathTransactions.Unlock()
	gate.refs--
	if gate.refs == 0 && workflowPathTransactions.locks[path] == gate {
		delete(workflowPathTransactions.locks, path)
	}
}
func pathTransactionRegistrySize() int {
	workflowPathTransactions.Lock()
	defer workflowPathTransactions.Unlock()
	return len(workflowPathTransactions.locks)
}

func NewStore(ctx context.Context, path string, lookup LookupEnv, providerValidate ProviderValidator) (*FileStore, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path: %w", err)
	}
	absolute, err = canonicalStorePath(absolute)
	if err != nil {
		return nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create workflow watcher: %w", err)
	}
	if err := watcher.Add(filepath.Dir(absolute)); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("watch workflow directory: %w", err)
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if providerValidate == nil {
		providerValidate = func(EffectiveConfig) []FieldError { return nil }
	}
	watchContext, cancel := context.WithCancel(context.Background())
	store := &FileStore{
		path:             filepath.Clean(absolute),
		pathMu:           retainPathTransaction(absolute),
		lookup:           lookup,
		providerValidate: providerValidate,
		atomic:           defaultAtomicOperations(),
		changes:          make(chan Change, 1),
		closed:           make(chan struct{}),
		stopping:         make(chan struct{}),
		watcher:          watcher,
		cancel:           cancel,
	}
	store.watchWG.Add(1)
	go store.watchLoop(watchContext)
	go func() {
		select {
		case <-ctx.Done():
			_ = store.Close()
		case <-store.closed:
		}
	}()
	return store, nil
}

func (store *FileStore) Current() (Snapshot, bool) {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return cloneSnapshot(store.current), store.hasValue
}

func (store *FileStore) Load(ctx context.Context) (Snapshot, error) {
	if !store.beginOperation() {
		return Snapshot{}, ErrStoreClosed
	}
	defer store.endOperation()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := store.pathMu.acquire(ctx, store.stopping); err != nil {
		return Snapshot{}, err
	}
	defer store.pathMu.release()
	return store.loadStableLocked(ctx)
}

func (store *FileStore) loadStableLocked(ctx context.Context) (Snapshot, error) {
	for attempts := 0; attempts < 8; attempts++ {
		source, err := os.ReadFile(store.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Snapshot{}, fmt.Errorf("%w: workflow file is not present", ErrMissingWorkflow)
			}
			return Snapshot{}, fmt.Errorf("read workflow: %w", err)
		}
		sourceDigest := digestSource(source)
		snapshot, validation, candidateErr := store.snapshotFromSource(source)
		latest, err := os.ReadFile(store.path)
		if err != nil {
			continue
		}
		if digestSource(latest) != sourceDigest {
			if ctx.Err() != nil {
				return Snapshot{}, ctx.Err()
			}
			continue
		}
		if candidateErr != nil || !validation.Valid {
			if candidateErr != nil {
				return Snapshot{}, candidateErr
			}
			return Snapshot{}, &InvalidWorkflowError{Validation: validation}
		}
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		select {
		case <-store.stopping:
			return Snapshot{}, ErrStoreClosed
		default:
		}
		store.installSnapshot(snapshot, false)
		return snapshot, nil
	}
	return Snapshot{}, ErrSaveConflict
}

func (store *FileStore) Validate(ctx context.Context, source []byte) ValidationResult {
	if !store.beginOperation() {
		return safeValidation(ErrStoreClosed)
	}
	defer store.endOperation()
	if err := ctx.Err(); err != nil {
		return ValidationResult{Valid: false, FieldErrors: []FieldError{}, GlobalErrors: []SafeError{{Code: "validation_canceled", Message: "Workflow validation was canceled."}}}
	}
	_, validation, _ := store.snapshotFromSource(source)
	return validation
}

func (store *FileStore) snapshotFromSource(source []byte) (Snapshot, ValidationResult, error) {
	definition, err := Parse(store.path, source)
	if err != nil {
		return Snapshot{}, safeValidation(err), err
	}
	config, err := Resolve(store.path, definition, store.lookup)
	if err != nil {
		return Snapshot{}, safeValidation(err), err
	}
	attempt := 1
	if _, err := Render(definition, TemplateIssue{}, &attempt); err != nil {
		return Snapshot{}, safeValidation(err), err
	}
	fieldErrors := store.providerValidate(cloneConfig(config))
	validation := validResult(fieldErrors)
	snapshot := snapshotAt(store.path, source, definition, config)
	if !validation.Valid {
		return snapshot, validation, &InvalidWorkflowError{Validation: validation}
	}
	return snapshot, validation, nil
}

func (store *FileStore) installSnapshot(snapshot Snapshot, publish bool) {
	store.stateMu.Lock()
	changed := !store.hasValue || store.current.Digest != snapshot.Digest
	store.current = cloneSnapshot(snapshot)
	store.hasValue = true
	store.stateMu.Unlock()
	if publish && changed {
		store.publish(Change{Snapshot: cloneSnapshot(snapshot), Digest: snapshot.Digest, Validation: validResult(nil)})
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	copy := snapshot
	copy.Definition.FrontMatter = cloneYAMLNode(snapshot.Definition.FrontMatter)
	copy.Config = cloneConfig(snapshot.Config)
	return copy
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	return cloneYAMLNodeGraph(node, make(map[*yaml.Node]*yaml.Node))
}

func cloneYAMLNodeGraph(node *yaml.Node, seen map[*yaml.Node]*yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if copy, found := seen[node]; found {
		return copy
	}
	copy := *node
	copy.Alias = nil
	copy.Content = nil
	seen[node] = &copy
	copy.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		copy.Content[index] = cloneYAMLNodeGraph(child, seen)
	}
	copy.Alias = cloneYAMLNodeGraph(node.Alias, seen)
	return &copy
}

func cloneConfig(config EffectiveConfig) EffectiveConfig {
	copy := config
	copy.Tracker.Provider = cloneStringAnyMap(config.Tracker.Provider)
	copy.Tracker.RequiredLabels = append([]string(nil), config.Tracker.RequiredLabels...)
	copy.Tracker.ActiveStates = append([]string(nil), config.Tracker.ActiveStates...)
	copy.Tracker.TerminalStates = append([]string(nil), config.Tracker.TerminalStates...)
	copy.Agent.MaxConcurrentByState = make(map[string]int, len(config.Agent.MaxConcurrentByState))
	for state, limit := range config.Agent.MaxConcurrentByState {
		copy.Agent.MaxConcurrentByState[state] = limit
	}
	copy.Codex.ApprovalPolicy = cloneConfigValue(config.Codex.ApprovalPolicy)
	copy.Codex.TurnSandboxPolicy = cloneStringAnyMap(config.Codex.TurnSandboxPolicy)
	return copy
}

func cloneStringAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	copy := make(map[string]any, len(input))
	for key, value := range input {
		copy[key] = cloneConfigValue(value)
	}
	return copy
}

func cloneConfigValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneStringAnyMap(value)
	case []any:
		copy := make([]any, len(value))
		for index, item := range value {
			copy[index] = cloneConfigValue(item)
		}
		return copy
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}

func (store *FileStore) Changes() <-chan Change { return store.changes }

func (store *FileStore) publish(change Change) {
	store.publishMu.Lock()
	defer store.publishMu.Unlock()
	select {
	case store.changes <- change:
		return
	default:
	}
	select {
	case <-store.changes:
	default:
	}
	select {
	case store.changes <- change:
	default:
	}
}

func (store *FileStore) beginOperation() bool {
	store.lifecycleMu.Lock()
	defer store.lifecycleMu.Unlock()
	if store.closing {
		return false
	}
	store.active.Add(1)
	store.activeCount++
	return true
}

func (store *FileStore) endOperation() {
	store.active.Done()
	store.lifecycleMu.Lock()
	store.activeCount--
	release := store.closing && store.activeCount == 0
	store.lifecycleMu.Unlock()
	if release {
		store.releaseGateRef()
	}
}

func (store *FileStore) releaseGateRef() {
	store.gateRelease.Do(func() { releasePathTransaction(store.path, store.pathMu) })
}

func (store *FileStore) Close() error {
	store.closeOnce.Do(func() {
		store.lifecycleMu.Lock()
		store.closing = true
		close(store.stopping)
		store.lifecycleMu.Unlock()
		store.cancel()
		store.closeErr = store.watcher.Close()
		store.watchWG.Wait()
		store.publishMu.Lock()
		close(store.changes)
		store.publishMu.Unlock()
		close(store.closed)
		store.lifecycleMu.Lock()
		idle := store.activeCount == 0
		store.lifecycleMu.Unlock()
		if idle {
			store.releaseGateRef()
		}
	})
	return store.closeErr
}

func canonicalStorePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workflow path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	probe := absolute
	var suffix []string
	for {
		if _, e := os.Lstat(probe); e == nil {
			break
		} else if !errors.Is(e, os.ErrNotExist) {
			return "", fmt.Errorf("inspect workflow path: %w", e)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("resolve workflow path: no existing ancestor")
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", fmt.Errorf("resolve workflow symlinks: %w", err)
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
	}
	return filepath.Clean(resolved), nil
}

var _ Store = (*FileStore)(nil)
