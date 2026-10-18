package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/secrets"
)

// State is the lifecycle state of one upstream.
type State string

const (
	// StateDisabled means the operator turned the server off in config.
	StateDisabled State = "disabled"
	// StateStopped means the server is enabled but no process is running.
	StateStopped State = "stopped"
	// StateStarting means a connection attempt is in flight.
	StateStarting State = "starting"
	// StateReady means a live session exists.
	StateReady State = "ready"
	// StateError means the last connect attempt failed, or the session died.
	StateError State = "error"
)

// ErrUnknownServer reports an operation on a key that is not configured.
var ErrUnknownServer = errors.New("unknown upstream server")

// ErrBackoff reports that an upstream failed recently and its next attempt is
// still in the future. Callers that are merely refreshing state treat it as
// "nothing to do yet"; callers acting on a demand wait for the retry instead.
var ErrBackoff = errors.New("waiting to retry after a failure")

// defaultReapInterval is how often idle upstreams are considered for shutdown.
const defaultReapInterval = 20 * time.Second

// Options configures a Manager.
type Options struct {
	// Logger receives lifecycle events. Defaults to slog.Default().
	Logger *slog.Logger
	// Resolver resolves secret references. Defaults to secrets.New().
	Resolver *secrets.Resolver
	// StopGrace bounds how long a child process gets to exit on its own.
	StopGrace time.Duration
	// OnToolsChanged is called when an upstream reports that its tool list
	// changed. The manager refreshes the list before invoking it.
	OnToolsChanged func(key string)
	// ReapInterval overrides how often idle upstreams are checked.
	ReapInterval time.Duration
}

// Manager owns every upstream connection. It starts servers on demand, keeps a
// single shared session per server, restarts crashed servers with backoff, and
// shuts idle ones down.
type Manager struct {
	log            *slog.Logger
	resolver       *secrets.Resolver
	stopGrace      time.Duration
	onToolsChanged func(key string)
	reapInterval   time.Duration

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool

	wg         sync.WaitGroup
	reaperStop context.CancelFunc
}

// entry is the mutable state for one configured upstream.
type entry struct {
	key string

	mu          sync.Mutex
	def         Def
	conn        *Conn
	state       State
	lastErr     string
	failures    int
	nextAttempt time.Time
	lastUsed    time.Time
	inFlight    int

	// cachedTools and cachedInfo survive an idle shutdown. A proxy has to keep
	// publishing a server's tools even while its process is stopped, otherwise
	// the aggregated list would shrink every time something went idle.
	cachedTools []Tool
	cachedInfo  Info

	// starting is non-nil while a connect attempt is in progress. Waiters
	// block on it and then re-check, which collapses concurrent starts into
	// one process launch.
	starting chan struct{}

	// generation increments whenever the definition changes, so watchers
	// belonging to a superseded connection cannot clobber newer state.
	generation uint64
}

// NewManager builds a Manager and starts its idle reaper.
func NewManager(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	res := opts.Resolver
	if res == nil {
		res = secrets.New()
	}
	grace := opts.StopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}
	interval := opts.ReapInterval
	if interval <= 0 {
		interval = defaultReapInterval
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		log:            log,
		resolver:       res,
		stopGrace:      grace,
		onToolsChanged: opts.OnToolsChanged,
		reapInterval:   interval,
		entries:        map[string]*entry{},
		reaperStop:     cancel,
	}
	m.wg.Add(1)
	go m.reapLoop(ctx)
	return m
}

// SetServers reconciles the manager against a new set of definitions. Servers
// that disappeared are stopped and forgotten; servers whose settings changed
// have their live connection dropped so the new settings take effect on next
// use.
func (m *Manager) SetServers(defs map[string]Def) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	var stale []*entry

	for key, def := range defs {
		if e, ok := m.entries[key]; ok {
			if e.updateDef(def) {
				stale = append(stale, e)
			}
			continue
		}
		e := &entry{key: key, def: def}
		if !defEnabled(def) {
			e.state = StateDisabled
		} else {
			e.state = StateStopped
		}
		m.entries[key] = e
	}

	for key, e := range m.entries {
		if _, ok := defs[key]; !ok {
			delete(m.entries, key)
			stale = append(stale, e)
		}
	}
	m.mu.Unlock()

	for _, e := range stale {
		e.detach()
	}
}

// defEnabled reports whether an upstream should ever be started.
func defEnabled(def Def) bool {
	return def.Cfg != nil &&
		def.Cfg.IsEnabled() &&
		def.Cfg.EffectiveStartPolicy(def.Defaults) != config.StartNever
}

// updateDef swaps in a new definition, reporting whether the live connection
// must be dropped.
func (e *entry) updateDef(def Def) (dropConn bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	changed := !sameUpstream(e.def.Cfg, def.Cfg) || e.def.Defaults != def.Defaults
	e.def = def
	e.generation++
	if !defEnabled(def) {
		e.state = StateDisabled
	} else if e.state == StateDisabled {
		e.state = StateStopped
	}
	return changed && e.conn != nil
}

// detach closes a connection without disturbing the entry map.
func (e *entry) detach() {
	e.mu.Lock()
	conn := e.conn
	e.conn = nil
	e.generation++
	e.mu.Unlock()

	if conn != nil {
		go func() {
			if err := conn.Close(); err != nil {
				e.recordCloseError(err)
			}
		}()
	}
}

func (e *entry) recordCloseError(err error) {
	e.mu.Lock()
	e.lastErr = err.Error()
	e.mu.Unlock()
}

// Acquire returns a live connection, starting the upstream if needed. Callers
// must call Release when the work is done so the idle reaper can do its job.
//
// A caller acting on a demand waits out a scheduled retry, because the work it
// is about to do needs the connection. Use acquire with waitForRetry false for
// a refresh, where waiting would only delay whoever asked for it.
func (m *Manager) Acquire(ctx context.Context, key string) (*Conn, error) {
	return m.acquire(ctx, key, true)
}

// acquire is Acquire with the retry wait made optional. It returns ErrBackoff
// instead of waiting when the retry is not due yet.
func (m *Manager) acquire(ctx context.Context, key string, waitForRetry bool) (*Conn, error) {
	e, err := m.entry(key)
	if err != nil {
		return nil, err
	}
	conn, err := m.ensure(ctx, e, waitForRetry)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.inFlight++
	e.lastUsed = time.Now()
	e.mu.Unlock()
	return conn, nil
}

// Release records that a caller has finished with a connection.
func (m *Manager) Release(key string) {
	e, err := m.entry(key)
	if err != nil {
		return
	}
	e.mu.Lock()
	if e.inFlight > 0 {
		e.inFlight--
	}
	e.lastUsed = time.Now()
	e.mu.Unlock()
}

// entry looks up a live entry.
func (m *Manager) entry(key string) (*entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("manager is closed")
	}
	e, ok := m.entries[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownServer, key)
	}
	return e, nil
}

// ensure drives an entry to the ready state, collapsing concurrent callers
// into a single connect attempt.
func (m *Manager) ensure(ctx context.Context, e *entry, waitForRetry bool) (*Conn, error) {
	for {
		e.mu.Lock()

		if !defEnabled(e.def) {
			state := e.state
			e.mu.Unlock()
			if state == StateDisabled {
				return nil, fmt.Errorf("upstream %q is disabled", e.key)
			}
			return nil, fmt.Errorf("upstream %q is set to start policy %q", e.key, config.StartNever)
		}

		if e.conn != nil && e.state == StateReady {
			conn := e.conn
			e.mu.Unlock()
			return conn, nil
		}

		if e.starting != nil {
			wait := e.starting
			e.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		if wait := time.Until(e.nextAttempt); wait > 0 {
			if !waitForRetry {
				key := e.key
				e.mu.Unlock()
				return nil, fmt.Errorf("%s: %w, next attempt in %s", key, ErrBackoff, wait.Round(time.Second))
			}
			e.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}

		// This caller becomes the starter.
		done := make(chan struct{})
		e.starting = done
		e.state = StateStarting
		def := e.def
		gen := e.generation
		e.mu.Unlock()

		conn, err := m.connect(ctx, e.key, def)

		e.mu.Lock()
		superseded := e.generation != gen
		e.starting = nil
		close(done)

		switch {
		case superseded:
			// The definition changed while we were connecting. Discard the
			// result and let the next caller start the current version.
			e.mu.Unlock()
			if conn != nil {
				_ = conn.Close()
			}
			continue

		case err != nil:
			e.state = StateError
			e.lastErr = err.Error()
			e.failures++
			e.nextAttempt = time.Now().Add(m.backoffFor(e.failures, def))
			failures := e.failures
			e.mu.Unlock()
			m.log.Warn("upstream connect failed",
				"server", e.key, "failure", failures, "error", err)
			return nil, err

		default:
			e.conn = conn
			e.state = StateReady
			e.lastErr = ""
			e.failures = 0
			e.nextAttempt = time.Time{}
			e.lastUsed = time.Now()
			info := conn.Info()
			e.cachedInfo = info
			e.cachedTools = info.Tools
			e.mu.Unlock()

			m.log.Info("upstream connected",
				"server", e.key,
				"transport", info.Transport,
				"protocol", info.ProtocolVersion,
				"tools", len(info.Tools))

			m.wg.Add(1)
			go m.watch(e, conn, gen)
			return conn, nil
		}
	}
}

// connect performs one connection attempt.
func (m *Manager) connect(ctx context.Context, key string, def Def) (*Conn, error) {
	timeout := def.Cfg.EffectiveConnectTimeout(def.Defaults)

	// The context handed to the transport deliberately outlives this call.
	//
	// An SSE session *is* the hanging GET request, and a Streamable HTTP
	// session can hold a stream open for server-to-client messages, so
	// cancelling the context when connect returns — which is exactly what
	// deferring a cancel does — tears the session down the moment it is
	// established. The symptom is quietly awful: the connection is logged as
	// established, the tools are enumerated, and then every call fails with EOF
	// because the request underneath the session was cancelled.
	//
	// The handshake still has to be bounded, so the connect timeout is a timer
	// that is stopped once the transport is up.
	//
	// The caller's context is detached as well: a harness that disconnects
	// mid-connect should not take the upstream's session with it.
	attemptCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	timer := time.AfterFunc(timeout, cancel)

	conn, err := connectServer(attemptCtx, connectOptions{
		def:       def,
		resolver:  m.resolver,
		log:       m.log,
		stopGrace: m.stopGrace,
		onChanged: func() { m.handleToolsChanged(key) },
	})

	if !timer.Stop() {
		// The timer fired while we were connecting, so the context is
		// cancelled or about to be: nothing that came back is usable.
		if conn != nil {
			_ = conn.Close()
		}
		cancel()
		return nil, fmt.Errorf("connecting to %q: %w", key, context.DeadlineExceeded)
	}

	if err != nil {
		cancel()
		return nil, err
	}

	return conn, nil
}

// handleToolsChanged refreshes the cached tool list and notifies the owner.
func (m *Manager) handleToolsChanged(key string) {
	e, err := m.entry(key)
	if err != nil {
		return
	}

	e.mu.Lock()
	conn := e.conn
	e.mu.Unlock()
	if conn == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := conn.RefreshTools(ctx); err != nil {
		m.log.Warn("refreshing tools after a change notification failed",
			"server", key, "error", err)
		return
	}

	// Keep the idle-surviving cache in step with the live session.
	info := conn.Info()
	e.mu.Lock()
	e.cachedInfo = info
	e.cachedTools = info.Tools
	e.mu.Unlock()

	if m.onToolsChanged != nil {
		m.onToolsChanged(key)
	}
}

// watch observes a live connection and records an unexpected exit.
func (m *Manager) watch(e *entry, conn *Conn, gen uint64) {
	defer m.wg.Done()

	<-conn.Done()

	e.mu.Lock()
	if e.conn != conn || e.generation != gen {
		// Already superseded; nothing to do.
		e.mu.Unlock()
		_ = conn.Close()
		return
	}

	crashed := conn.proc != nil && !conn.proc.Stopped()
	var reason string
	if crashed {
		reason = fmt.Sprintf("process exited unexpectedly: %v", conn.proc.WaitErr())
		e.state = StateError
		e.lastErr = reason
		e.failures++
		e.nextAttempt = time.Now().Add(m.backoffFor(e.failures, e.def))
	} else {
		e.state = StateStopped
	}
	e.conn = nil
	e.mu.Unlock()

	if crashed {
		m.log.Warn("upstream exited unexpectedly", "server", e.key, "error", reason)
	}
	_ = conn.Close()
}

// backoffFor computes the delay before the next attempt after n failures.
func (m *Manager) backoffFor(failures int, def Def) time.Duration {
	b := def.Defaults.EffectiveBackoff()
	d := time.Duration(b.Initial)
	max := time.Duration(b.Max)
	for i := 1; i < failures; i++ {
		next := time.Duration(float64(d) * b.Factor)
		if next >= max {
			return max
		}
		d = next
	}
	if d > max {
		return max
	}
	return d
}

// reapLoop stops upstreams that have been idle for longer than their timeout.
func (m *Manager) reapLoop(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.reapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

// reapIdle stops any ready upstream that has been unused past its idle
// timeout and has no calls in flight.
func (m *Manager) reapIdle() {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	now := time.Now()
	for _, e := range entries {
		e.mu.Lock()
		if e.state != StateReady || e.conn == nil || e.inFlight > 0 {
			e.mu.Unlock()
			continue
		}
		idle := e.def.Cfg.EffectiveIdleTimeout(e.def.Defaults)
		if idle <= 0 || now.Sub(e.lastUsed) < idle {
			e.mu.Unlock()
			continue
		}
		conn := e.conn
		e.conn = nil
		e.state = StateStopped
		e.mu.Unlock()

		m.log.Info("stopping idle upstream", "server", e.key, "idle", idle)
		go func() {
			if err := conn.Close(); err != nil {
				m.log.Warn("closing an idle upstream failed", "server", e.key, "error", err)
			}
		}()
	}
}

// Restart drops the current connection and connects again.
func (m *Manager) Restart(ctx context.Context, key string) error {
	e, err := m.entry(key)
	if err != nil {
		return err
	}

	e.mu.Lock()
	conn := e.conn
	e.conn = nil
	e.state = StateStopped
	e.failures = 0
	e.nextAttempt = time.Time{}
	e.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	_, err = m.Acquire(ctx, key)
	if err == nil {
		m.Release(key)
	}
	return err
}

// Stop shuts an upstream down if it is running.
func (m *Manager) Stop(key string) error {
	e, err := m.entry(key)
	if err != nil {
		return err
	}

	e.mu.Lock()
	conn := e.conn
	e.conn = nil
	if e.state != StateDisabled {
		e.state = StateStopped
	}
	e.mu.Unlock()

	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Enumerate connects enabled upstreams so the aggregated tool list is
// complete shortly after start.
//
// A proxy cannot publish tools it has never seen, so enumeration has to happen
// at startup rather than only on first call. Servers marked eager block until
// they are connected; lazy servers connect in the background so boot stays
// fast. Idleness is still reclaimed later by the reaper, and the cached tool
// list survives that shutdown.
// Enumerate connects every enabled upstream so the aggregated tool list is
// complete.
//
// Eager servers are connected concurrently and this call blocks until they are
// done, so start-up time is bounded by the slowest server rather than the sum
// of all of them. Lazy servers are connected in the background: they still
// start at boot, but a slow one delays only its own tools appearing, not the
// proxy becoming usable.
func (m *Manager) Enumerate(ctx context.Context) {
	m.mu.Lock()
	var eager, lazy []string
	for key, e := range m.entries {
		e.mu.Lock()
		def := e.def
		e.mu.Unlock()
		if !defEnabled(def) {
			continue
		}
		if def.Cfg.EffectiveStartPolicy(def.Defaults) == config.StartEager {
			eager = append(eager, key)
		} else {
			lazy = append(lazy, key)
		}
	}
	m.mu.Unlock()

	sort.Strings(eager)
	sort.Strings(lazy)

	var wg sync.WaitGroup
	for _, key := range eager {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := m.connectAndRelease(ctx, key); err != nil {
				if errors.Is(err, ErrBackoff) {
					m.log.Debug("skipping enumeration of an upstream that is waiting to retry", "server", key, "error", err)
					return
				}
				m.log.Warn("connecting an upstream failed", "server", key, "error", err)
			}
		}(key)
	}
	wg.Wait()

	for _, key := range lazy {
		m.wg.Add(1)
		go func(key string) {
			defer m.wg.Done()
			if err := m.connectAndRelease(ctx, key); err != nil {
				m.log.Debug("background enumeration failed", "server", key, "error", err)
			}
		}(key)
	}
}

// connectAndRelease connects an upstream and immediately marks it unused, so
// the reaper can shut it down once its idle timeout passes.
//
// This is a refresh rather than a demand, so an upstream that is waiting out a
// backoff is skipped: its retry is already scheduled, and blocking on the timer
// would stall whatever triggered the enumeration — a reload after a config
// save, for instance.
func (m *Manager) connectAndRelease(ctx context.Context, key string) error {
	if _, err := m.acquire(ctx, key, false); err != nil {
		return err
	}
	m.Release(key)
	return nil
}

// Get returns a live connection without starting one.
func (m *Manager) Get(key string) (*Conn, bool) {
	e, err := m.entry(key)
	if err != nil {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil {
		return nil, false
	}
	return e.conn, true
}

// Test connects to an upstream definition without registering it, returning
// what the server reports. The connection is closed before returning. This
// backs the "test" button in the UI.
func (m *Manager) Test(ctx context.Context, def Def) (*Info, error) {
	conn, err := m.connect(ctx, def.Key, def)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	info := conn.Info()
	return &info, nil
}

// Close stops every upstream and shuts the reaper down.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.entries = map[string]*entry{}
	m.mu.Unlock()

	m.reaperStop()

	var wg sync.WaitGroup
	for _, e := range entries {
		e.mu.Lock()
		conn := e.conn
		e.conn = nil
		e.mu.Unlock()
		if conn == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = conn.Close()
		}()
	}
	wg.Wait()

	// Wait for watchers and the reaper to finish, but do not hang forever if a
	// misbehaving child refuses to die.
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(m.stopGrace * 2):
		m.log.Warn("shutdown timed out waiting for background workers")
	}
	return nil
}
