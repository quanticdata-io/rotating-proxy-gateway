// Package pool chooses an upstream for each connection: rotation strategy,
// sticky sessions, and backoff for upstreams that fail.
package pool

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/upstream"
)

// Strategy picks the next upstream when no sticky session applies.
type Strategy string

// Strategies.
const (
	RoundRobin Strategy = "round-robin"
	Random     Strategy = "random"
	LeastConn  Strategy = "least-conn"
)

// ParseStrategy validates a strategy name.
func ParseStrategy(s string) (Strategy, error) {
	switch Strategy(s) {
	case RoundRobin, Random, LeastConn:
		return Strategy(s), nil
	}
	return "", fmt.Errorf("unknown strategy %q (round-robin, random, least-conn)", s)
}

// Options configure a Pool.
type Options struct {
	Strategy    Strategy
	StickyTTL   time.Duration // 0: a session keeps its upstream until it fails
	Cooldown    time.Duration // first rest after a failure, doubled per failure in a row
	MaxCooldown time.Duration
	MaxFailures int // failures in a row before an upstream is dead; 0 = never
	Now         func() time.Time
	Rand        func(n int) int
}

// DefaultOptions are the gateway defaults.
func DefaultOptions() Options {
	return Options{
		Strategy:    RoundRobin,
		StickyTTL:   10 * time.Minute,
		Cooldown:    30 * time.Second,
		MaxCooldown: 10 * time.Minute,
		MaxFailures: 5,
	}
}

// ErrNoUpstream is returned when every upstream is resting or dead.
var ErrNoUpstream = errors.New("no upstream available")

type entry struct {
	up            upstream.Upstream
	active        int
	uses          int64
	successes     int64
	failures      int64
	streak        int
	cooldownUntil time.Time
	dead          bool
	lastError     string
}

type binding struct {
	index   int
	expires time.Time // zero: no expiry
}

// Pool is safe for concurrent use.
type Pool struct {
	opts     Options
	mu       sync.Mutex
	entries  []*entry
	cursor   int
	sessions map[string]binding
}

// New builds a pool. Duplicate upstreams are kept once.
func New(ups []upstream.Upstream, opts Options) (*Pool, error) {
	if len(ups) == 0 {
		return nil, errors.New("no upstream proxies given")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.IntN
	}
	if opts.Strategy == "" {
		opts.Strategy = RoundRobin
	}
	if opts.MaxCooldown < opts.Cooldown {
		opts.MaxCooldown = opts.Cooldown
	}
	p := &Pool{opts: opts, sessions: map[string]binding{}}
	seen := map[string]bool{}
	for _, u := range ups {
		if seen[u.Key()+":"+u.Password] {
			continue
		}
		seen[u.Key()+":"+u.Password] = true
		p.entries = append(p.entries, &entry{up: u})
	}
	return p, nil
}

// Lease is one use of an upstream. Call Done exactly once.
type Lease struct {
	Upstream upstream.Upstream
	pool     *Pool
	index    int
	session  string
	once     sync.Once
}

// Done reports the outcome. err == nil is a success.
func (l *Lease) Done(err error) {
	l.once.Do(func() { l.pool.done(l, err) })
}

// LeaseFor leases one specific upstream whatever its state (used by health
// checks, so a dead upstream that answers again comes back). It returns nil
// for an upstream that is not in the pool.
func (p *Pool) LeaseFor(u upstream.Upstream) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.up == u {
			return p.lease(i, "", p.opts.Now())
		}
	}
	return nil
}

// Len is the number of upstreams.
func (p *Pool) Len() int { return len(p.entries) }

// Acquire picks an upstream. A non-empty session keeps the same upstream until
// it fails or the binding expires. exclude lists upstream keys already tried
// for this connection.
func (p *Pool) Acquire(session string, exclude map[string]bool) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	if session != "" {
		if b, ok := p.sessions[session]; ok {
			e := p.entries[b.index]
			expired := !b.expires.IsZero() && !now.Before(b.expires)
			if !expired && p.usable(e, now) && !exclude[e.up.Key()] {
				return p.lease(b.index, session, now), nil
			}
			delete(p.sessions, session)
		}
	}
	idx, err := p.pick(now, exclude)
	if err != nil {
		return nil, err
	}
	if session != "" {
		var exp time.Time
		if p.opts.StickyTTL > 0 {
			exp = now.Add(p.opts.StickyTTL)
		}
		p.sessions[session] = binding{index: idx, expires: exp}
	}
	return p.lease(idx, session, now), nil
}

func (p *Pool) lease(idx int, session string, _ time.Time) *Lease {
	e := p.entries[idx]
	e.active++
	e.uses++
	return &Lease{Upstream: e.up, pool: p, index: idx, session: session}
}

func (p *Pool) usable(e *entry, now time.Time) bool {
	return !e.dead && !now.Before(e.cooldownUntil)
}

func (p *Pool) pick(now time.Time, exclude map[string]bool) (int, error) {
	var candidates []int
	for i, e := range p.entries {
		if p.usable(e, now) && !exclude[e.up.Key()] {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return 0, p.noUpstream(now)
	}
	switch p.opts.Strategy {
	case Random:
		return candidates[p.opts.Rand(len(candidates))], nil
	case LeastConn:
		sort.SliceStable(candidates, func(a, b int) bool {
			ea, eb := p.entries[candidates[a]], p.entries[candidates[b]]
			if ea.active != eb.active {
				return ea.active < eb.active
			}
			return ea.uses < eb.uses
		})
		return candidates[0], nil
	default:
		n := len(p.entries)
		for step := 0; step < n; step++ {
			i := (p.cursor + step) % n
			e := p.entries[i]
			if p.usable(e, now) && !exclude[e.up.Key()] {
				p.cursor = (i + 1) % n
				return i, nil
			}
		}
		return 0, p.noUpstream(now)
	}
}

func (p *Pool) noUpstream(now time.Time) error {
	var soonest time.Duration = -1
	for _, e := range p.entries {
		if e.dead {
			continue
		}
		if wait := e.cooldownUntil.Sub(now); soonest < 0 || wait < soonest {
			soonest = wait
		}
	}
	if soonest < 0 {
		return fmt.Errorf("%w: every upstream is dead", ErrNoUpstream)
	}
	if soonest <= 0 {
		return fmt.Errorf("%w: every usable upstream was already tried for this connection", ErrNoUpstream)
	}
	return fmt.Errorf("%w: every upstream is cooling down, next in %s", ErrNoUpstream, soonest.Round(time.Second))
}

func (p *Pool) done(l *Lease, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[l.index]
	e.active--
	if err == nil {
		e.successes++
		e.streak = 0
		e.cooldownUntil = time.Time{}
		e.dead = false // only health checks lease dead upstreams
		return
	}
	e.failures++
	e.streak++
	e.lastError = err.Error()
	for key, b := range p.sessions {
		if b.index == l.index {
			delete(p.sessions, key)
		}
	}
	if p.opts.MaxFailures > 0 && e.streak >= p.opts.MaxFailures {
		e.dead = true
		return
	}
	shift := e.streak - 1
	if shift > 20 {
		shift = 20
	}
	wait := p.opts.Cooldown << shift
	if wait > p.opts.MaxCooldown || wait <= 0 {
		wait = p.opts.MaxCooldown
	}
	e.cooldownUntil = p.opts.Now().Add(wait)
}

// Revive clears the dead flag and cooldown of every upstream.
func (p *Pool) Revive() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		e.dead, e.streak, e.cooldownUntil = false, 0, time.Time{}
	}
}

// Status of one upstream, for the admin endpoint. The password is masked.
type Status struct {
	Upstream      string     `json:"upstream"`
	State         string     `json:"state"` // available, cooling, dead
	Active        int        `json:"active"`
	Uses          int64      `json:"uses"`
	Successes     int64      `json:"successes"`
	Failures      int64      `json:"failures"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// Snapshot returns the state of every upstream and the number of live sessions.
func (p *Pool) Snapshot() ([]Status, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	out := make([]Status, 0, len(p.entries))
	for _, e := range p.entries {
		s := Status{
			Upstream: e.up.Redacted(), Active: e.active, Uses: e.uses,
			Successes: e.successes, Failures: e.failures, LastError: e.lastError,
		}
		switch {
		case e.dead:
			s.State = "dead"
		case now.Before(e.cooldownUntil):
			s.State = "cooling"
			t := e.cooldownUntil
			s.CooldownUntil = &t
		default:
			s.State = "available"
		}
		out = append(out, s)
	}
	live := 0
	for _, b := range p.sessions {
		if b.expires.IsZero() || now.Before(b.expires) {
			live++
		}
	}
	return out, live
}

// Upstreams returns the upstreams in pool order.
func (p *Pool) Upstreams() []upstream.Upstream {
	out := make([]upstream.Upstream, len(p.entries))
	for i, e := range p.entries {
		out[i] = e.up
	}
	return out
}
