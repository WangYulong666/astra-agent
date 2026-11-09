package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

type preservingProvider struct {
	*fakeProvider
	paused []string
}

func (p *preservingProvider) Suspend(_ context.Context, id string) error {
	p.paused = append(p.paused, id)
	return nil
}

type runStore struct {
	*fakeStore
	runs   []store.Run
	runErr error
}

func (s runStore) ListRuns(context.Context, string) ([]store.Run, error) { return s.runs, s.runErr }

func TestIdleSweepKeepsUnfinishedRuns(t *testing.T) {
	for _, status := range []string{store.RunQueued, store.RunRunning, store.RunWaitingApproval} {
		t.Run(status, func(t *testing.T) {
			p := &fakeProvider{infos: []sandbox.Info{info("busy", time.Hour)}}
			st := runStore{fakeStore: newFakeStore(session("busy", "t1", store.SessionActive, time.Hour)), runs: []store.Run{{Status: status}}}
			rep, err := newReaper(Policy{IdleTimeout: time.Minute}, p, st).Sweep(context.Background())
			if err != nil || rep.Kept != 1 || len(p.destroyed()) != 0 {
				t.Fatalf("busy run reclaimed: %+v, %v", rep, err)
			}
		})
	}
}
func TestIdleSweepFailsClosedOnRunReadError(t *testing.T) {
	p := &fakeProvider{infos: []sandbox.Info{info("busy", time.Hour)}}
	st := runStore{fakeStore: newFakeStore(session("busy", "t1", store.SessionActive, time.Hour)), runErr: errors.New("store unavailable")}
	rep, err := newReaper(Policy{IdleTimeout: time.Minute}, p, st).Sweep(context.Background())
	if err != nil || len(rep.Errors) != 1 || len(p.destroyed()) != 0 {
		t.Fatalf("read failure must not reclaim: %+v, %v", rep, err)
	}
}
func TestLocalReclaimPausesOwnedSandbox(t *testing.T) {
	for _, pol := range []Policy{{IdleTimeout: time.Minute}, {MaxAge: time.Minute}} {
		p := &preservingProvider{fakeProvider: &fakeProvider{infos: []sandbox.Info{info("owned", time.Hour), info("orphan", time.Hour)}}}
		st := newFakeStore(session("owned", "t1", store.SessionActive, time.Hour))
		rep, err := newReaper(pol, p, st).Sweep(context.Background())
		if err != nil || rep.Paused != 1 || rep.Destroyed != 1 || len(p.paused) != 1 || p.paused[0] != "owned" {
			t.Fatalf("wrong reclaim action: %+v %v paused=%v", rep, err, p.paused)
		}
		if got := p.destroyed(); len(got) != 1 || got[0] != "orphan" {
			t.Fatalf("owned data destroyed: %v", got)
		}
	}
}
func TestSweepKeepsPausedSandbox(t *testing.T) {
	in := info("owned", time.Hour)
	in.State = sandbox.StatePaused
	p := &fakeProvider{infos: []sandbox.Info{in}}
	rep, err := newReaper(Policy{IdleTimeout: time.Minute, MaxAge: time.Minute}, p, newFakeStore(session("owned", "t1", store.SessionActive, time.Hour))).Sweep(context.Background())
	if err != nil || rep.Kept != 1 || len(p.destroyed()) != 0 {
		t.Fatalf("paused files reclaimed: %+v %v", rep, err)
	}
}
func TestIdleSweepRechecksActivityUnderGuard(t *testing.T) {
	p := &fakeProvider{infos: []sandbox.Info{info("owned", time.Hour)}}
	st := newFakeStore(session("owned", "t1", store.SessionActive, time.Hour))
	r := newReaper(Policy{IdleTimeout: time.Minute}, p, st)
	r.deps.WithIdleSession = func(ctx context.Context, id string, fn func() error) (bool, error) {
		latest, _ := st.GetSession(ctx, id)
		latest.LastActiveAt = fixedNow
		_ = st.PutSession(ctx, latest)
		return true, fn()
	}
	rep, err := r.Sweep(context.Background())
	if err != nil || rep.Kept != 1 || len(p.destroyed()) != 0 {
		t.Fatalf("fresh activity reclaimed: %+v %v", rep, err)
	}
}
