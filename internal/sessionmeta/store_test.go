package sessionmeta

import (
	"path/filepath"
	"testing"
	"time"
)

const sevenDays = 7 * 24 * time.Hour

func newTestStore(t *testing.T) *Store {
	return New(filepath.Join(t.TempDir(), "sessions.json"), sevenDays)
}

func TestIsArchived_DefaultsToActivityWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	if s.IsArchived("a", now.Add(-1*time.Hour), now) {
		t.Errorf("fresh session should be visible")
	}
	if !s.IsArchived("b", now.Add(-8*24*time.Hour), now) {
		t.Errorf("8-day-old session should auto-archive")
	}
	if s.IsArchived("c", time.Time{}, now) {
		t.Errorf("zero last_event should not auto-archive (treat as unknown)")
	}
}

func TestIsArchived_ManualOverridesActivity(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	s.Archive("fresh")
	if !s.IsArchived("fresh", now, now) {
		t.Errorf("manual archive should override fresh activity")
	}

	s.Unarchive("stale", stale, now)
	if s.IsArchived("stale", stale, now) {
		t.Errorf("unarchive on stale should leave it visible")
	}
}

func TestUnarchive_FreshSessionDeletesEntry(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	fresh := now.Add(-1 * time.Hour)
	stale := now.Add(-30 * 24 * time.Hour)

	s.Archive("a")
	s.Unarchive("a", fresh, now)

	if s.IsArchived("a", fresh, now) {
		t.Errorf("a should be visible after unarchive on fresh")
	}
	if !s.IsArchived("a", stale, now) {
		t.Errorf("fresh-unarchive must not leave an override; stale should auto-archive again")
	}
}

func TestAutoArchiveDisabled(t *testing.T) {
	s := New("", 0)
	now := time.Now()
	stale := now.Add(-365 * 24 * time.Hour)

	if s.IsArchived("untouched", stale, now) {
		t.Errorf("autoAfter=0 must never auto-archive")
	}

	s.Archive("manual")
	if !s.IsArchived("manual", now, now) {
		t.Errorf("manual archive should still take effect with autoAfter=0")
	}

	s.Unarchive("manual", stale, now)
	if s.IsArchived("manual", stale, now) {
		t.Errorf("unarchive should clear the manual archive even with autoAfter=0")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	s1 := New(path, sevenDays)
	s1.Archive("a")
	s1.Unarchive("b", stale, now)
	s1.Archive("c")
	s1.Pin("x")

	s2 := New(path, sevenDays)
	if !s2.IsArchived("a", now, now) {
		t.Errorf("a should be archived after rehydrate")
	}
	if s2.IsArchived("b", stale, now) {
		t.Errorf("b should be visible after rehydrate (DecisionShown survives)")
	}
	if !s2.IsArchived("c", now, now) {
		t.Errorf("c should be archived after rehydrate")
	}
	if !s2.IsPinned("x") {
		t.Errorf("x should be pinned after rehydrate")
	}
}

func TestArchive_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	s := New(path, sevenDays)
	s.Archive("a")
	s.Archive("a")
	s.Unarchive("a", stale, now)
	s.Unarchive("a", stale, now)

	s2 := New(path, sevenDays)
	if s2.IsArchived("a", stale, now) {
		t.Errorf("after Archive → Unarchive on stale, session should stay visible")
	}
}

func TestPin(t *testing.T) {
	s := newTestStore(t)

	if s.IsPinned("a") {
		t.Errorf("should not be pinned by default")
	}
	s.Pin("a")
	if !s.IsPinned("a") {
		t.Errorf("should be pinned after Pin")
	}
	s.Unpin("a")
	if s.IsPinned("a") {
		t.Errorf("should not be pinned after Unpin")
	}
}

func TestPin_OverridesArchive(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	s.Pin("auto")
	if s.IsArchived("auto", stale, now) {
		t.Errorf("pinned session should not auto-archive when stale")
	}

	s.Archive("manual")
	s.Pin("manual")
	if s.IsArchived("manual", now, now) {
		t.Errorf("pin should overlay an earlier manual archive")
	}
	s.Unpin("manual")
	if !s.IsArchived("manual", now, now) {
		t.Errorf("unpin should restore the earlier manual archive")
	}
}

func TestArchive_Unpins(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	s.Pin("a")
	s.Archive("a")
	if s.IsPinned("a") || !s.IsArchived("a", now, now) {
		t.Errorf("archiving a pinned session should drop the pin and take effect")
	}
}

func TestForget(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	s.Archive("a")
	s.Pin("a")
	s.Forget("a")

	if s.IsArchived("a", now, now) {
		t.Errorf("archive state should be cleared after Forget")
	}
	if s.IsPinned("a") {
		t.Errorf("pin state should be cleared after Forget")
	}
}
