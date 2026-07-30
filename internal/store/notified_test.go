package store

import (
	"path/filepath"
	"testing"
)

// Notification state lives in the database, not in memory: a container restart
// must not re-send every outstanding problem, which is exactly when someone is
// most likely to be restarting it.
func TestNotifiedStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotified("Database integrity check FAILED", "fault"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	got, err := reopened.NotifiedFindings()
	if err != nil {
		t.Fatal(err)
	}
	if got["Database integrity check FAILED"] != "fault" {
		t.Errorf("state did not survive a restart: %v", got)
	}
}

func TestClearNotifiedAllowsRenotification(t *testing.T) {
	st := openTemp(t)

	if err := st.MarkNotified("Search indexes report corruption", "warn"); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearNotified("Search indexes report corruption"); err != nil {
		t.Fatal(err)
	}

	got, err := st.NotifiedFindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty so a recurrence is reported again", got)
	}
}

// Marking twice must not fail or duplicate.
func TestMarkNotifiedIsIdempotent(t *testing.T) {
	st := openTemp(t)
	for range 3 {
		if err := st.MarkNotified("a", "warn"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.NotifiedFindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d rows, want 1", len(got))
	}
}
