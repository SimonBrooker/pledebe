package store

import (
	"fmt"
	"time"
)

// Notifications fire on change, not on state, so pledebe has to remember what it
// has already reported. Without this a corrupt database would produce an email
// every fifteen minutes.
//
// Kept in the history database rather than in memory: a container restart must
// not re-send everything, which is exactly when someone is most likely to be
// restarting it.
const notifiedSchema = `
CREATE TABLE IF NOT EXISTS notified (
    title       TEXT PRIMARY KEY,
    level       TEXT NOT NULL,
    notified_at DATETIME NOT NULL
);
`

// NotifiedFindings returns the finding titles already reported, mapped to the
// level they were reported at.
func (s *Store) NotifiedFindings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT title, level FROM notified`)
	if err != nil {
		return nil, fmt.Errorf("query notified: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var title, level string
		if err := rows.Scan(&title, &level); err != nil {
			return nil, err
		}
		out[title] = level
	}
	return out, rows.Err()
}

// MarkNotified records that a finding has been reported.
func (s *Store) MarkNotified(title, level string) error {
	_, err := s.db.Exec(`
        INSERT INTO notified (title, level, notified_at) VALUES (?,?,?)
        ON CONFLICT(title) DO UPDATE SET level=excluded.level,
                                        notified_at=excluded.notified_at`,
		title, level, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}

// ClearNotified forgets a finding, so if it returns it is reported again.
func (s *Store) ClearNotified(title string) error {
	_, err := s.db.Exec(`DELETE FROM notified WHERE title = ?`, title)
	if err != nil {
		return fmt.Errorf("clear notified: %w", err)
	}
	return nil
}
