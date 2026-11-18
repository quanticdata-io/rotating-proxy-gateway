package pool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/upstream"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func ups(n int) []upstream.Upstream {
	out := make([]upstream.Upstream, n)
	for i := range out {
		out[i] = upstream.Upstream{Scheme: upstream.HTTP, Host: fmt.Sprintf("h%d", i), Port: 1000 + i}
	}
	return out
}

func newPool(t *testing.T, n int, mod func(*Options)) (*Pool, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	o := DefaultOptions()
	o.Now = c.now
	if mod != nil {
		mod(&o)
	}
	p, err := New(ups(n), o)
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

func host(t *testing.T, p *Pool, session string) string {
	t.Helper()
	l, err := p.Acquire(session, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Done(nil)
	return l.Upstream.Host
}

func TestRoundRobin(t *testing.T) {
	p, _ := newPool(t, 3, nil)
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, host(t, p, ""))
	}
	if fmt.Sprint(got) != "[h0 h1 h2 h0 h1 h2]" {
		t.Fatal(got)
	}
}

func TestStickySessionAndTTL(t *testing.T) {
	p, c := newPool(t, 3, func(o *Options) { o.StickyTTL = time.Minute })
	first := host(t, p, "a")
	for i := 0; i < 5; i++ {
		if h := host(t, p, "a"); h != first {
			t.Fatalf("session moved to %s", h)
		}
	}
	if host(t, p, "b") == first {
		t.Fatal("second session got the same upstream with free ones left")
	}
	c.advance(61 * time.Second)
	if _, live := p.Snapshot(); live != 0 {
		t.Fatalf("expired sessions still counted: %d", live)
	}
}

func TestFailureBackoffAndDead(t *testing.T) {
	p, c := newPool(t, 1, func(o *Options) {
		o.Cooldown, o.MaxCooldown, o.MaxFailures = 10*time.Second, 25*time.Second, 4
	})
	fail := func() {
		l, err := p.Acquire("", nil)
		if err != nil {
			t.Fatal(err)
		}
		l.Done(errors.New("boom"))
	}
	for _, w := range []time.Duration{10 * time.Second, 20 * time.Second, 25 * time.Second} {
		fail()
		c.advance(w - time.Second)
		if _, err := p.Acquire("", nil); !errors.Is(err, ErrNoUpstream) {
			t.Fatalf("usable before the end of a %s cooldown", w)
		}
		c.advance(time.Second)
	}
	fail() // fourth failure in a row: dead
	c.advance(time.Hour)
	_, err := p.Acquire("", nil)
	if !errors.Is(err, ErrNoUpstream) || err.Error() != "no upstream available: every upstream is dead" {
		t.Fatalf("want dead, got %v", err)
	}
	st, _ := p.Snapshot()
	if st[0].State != "dead" || st[0].Failures != 4 || st[0].LastError != "boom" {
		t.Fatalf("%+v", st[0])
	}
	if l := p.LeaseFor(p.Upstreams()[0]); l != nil {
		l.Done(nil) // a health check success revives it
	}
	if host(t, p, "") != "h0" {
		t.Fatal("not revived")
	}
}

func TestFailureMovesSession(t *testing.T) {
	p, _ := newPool(t, 2, nil)
	l, _ := p.Acquire("s", nil)
	first := l.Upstream.Host
	l.Done(errors.New("down"))
	if h := host(t, p, "s"); h == first {
		t.Fatal("session stayed on the failed upstream")
	}
}

func TestExclude(t *testing.T) {
	p, _ := newPool(t, 2, nil)
	all := map[string]bool{}
	for _, u := range p.Upstreams() {
		all[u.Key()] = true
	}
	if _, err := p.Acquire("", all); !errors.Is(err, ErrNoUpstream) {
		t.Fatalf("want ErrNoUpstream, got %v", err)
	}
}

func TestLeastConn(t *testing.T) {
	p, _ := newPool(t, 3, func(o *Options) { o.Strategy = LeastConn })
	held, _ := p.Acquire("", nil) // h0 stays active
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		seen[host(t, p, "")]++
	}
	held.Done(nil)
	if seen["h0"] != 0 {
		t.Fatalf("least-conn picked the busy upstream: %v", seen)
	}
}

func TestRandomAndParse(t *testing.T) {
	n := 0
	p, _ := newPool(t, 3, func(o *Options) {
		o.Strategy = Random
		o.Rand = func(k int) int { n++; return k - 1 }
	})
	if host(t, p, "") != "h2" || n != 1 {
		t.Fatal("random strategy not used")
	}
	if _, err := ParseStrategy("weighted"); err == nil {
		t.Fatal("want an error")
	}
	if _, err := New(nil, DefaultOptions()); err == nil {
		t.Fatal("want an error for an empty pool")
	}
}

func TestConcurrentUse(t *testing.T) {
	p, _ := newPool(t, 5, func(o *Options) { o.MaxFailures = 0; o.Cooldown = 0 })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				l, err := p.Acquire(fmt.Sprint(i%4), nil)
				if err != nil {
					continue
				}
				if i%7 == 0 {
					l.Done(errors.New("x"))
				} else {
					l.Done(nil)
				}
			}
		}()
	}
	wg.Wait()
	st, _ := p.Snapshot()
	for _, s := range st {
		if s.Active != 0 {
			t.Fatalf("leaked lease: %+v", s)
		}
	}
}
