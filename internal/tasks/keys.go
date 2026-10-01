package tasks

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// taskKeyAlphabet drops 0/1/o/l/i so a key stays unambiguous when it is read
// aloud or copied by hand. 31 symbols over keySuffixLength positions give
// 923521 keys per queue, which is why generation can rely on the task_key
// UNIQUE constraint instead of a counter.
const taskKeyAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"

const keySuffixLength = 4

// newTaskKeySuffix returns a random lowercase suffix for a task key.
func newTaskKeySuffix() (string, error) {
	buf := make([]byte, keySuffixLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, keySuffixLength)
	for i, b := range buf {
		out[i] = taskKeyAlphabet[int(b)%len(taskKeyAlphabet)]
	}
	return string(out), nil
}

// NormalizeKey puts a key into its canonical form: an uppercase queue prefix
// and a lowercase random suffix. Keys reach the daemon from CLIs, branch names
// and pasted text, so lookup normalizes rather than rejecting a differently
// cased spelling. A value without a separator is only trimmed and uppercased,
// keeping legacy numeric keys unchanged.
func NormalizeKey(key string) string {
	key = strings.TrimSpace(key)
	prefix, suffix, ok := strings.Cut(key, "-")
	if !ok {
		return strings.ToUpper(key)
	}
	return strings.ToUpper(prefix) + "-" + strings.ToLower(suffix)
}

// legacyKeySelect finds tasks that still carry a numeric autoincrement suffix.
const legacyKeySelect = `
SELECT id, task_key, queue_prefix FROM tasks
WHERE substr(task_key, length(queue_prefix) + 2) GLOB '[0-9]*'
  AND substr(task_key, length(queue_prefix) + 2) NOT GLOB '*[^0-9]*'
ORDER BY id`

// MigrateLegacyKeys rewrites every remaining numeric task key to a random one
// and records the old key as a permanent alias. It runs once per database and
// records that in task_legacy_keys_migrated: a random suffix can be all digits
// too, so a later run would mistake such a key for a legacy one and rename it.
func MigrateLegacyKeys(db *sql.DB, now func() time.Time) error {
	var done int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_legacy_keys_migrated`).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		return nil
	}
	rows, err := db.Query(legacyKeySelect)
	if err != nil {
		return err
	}
	type legacy struct {
		id    int64
		key   string
		queue string
	}
	var pending []legacy
	for rows.Next() {
		var item legacy
		if err := rows.Scan(&item.id, &item.key, &item.queue); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO task_legacy_keys_migrated(id, migrated_at) VALUES (1, ?)`, stamp); err != nil {
		return err
	}
	for _, item := range pending {
		key, err := reserveTaskKey(tx, item.queue)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET task_key = ? WHERE id = ?`, key, item.id); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO task_key_aliases(old_key, task_id, created_at) VALUES (?, ?, ?)`,
			item.key, item.id, stamp); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE agents SET current_goal_task_key = ? WHERE current_goal_task_key = ?`,
			key, item.key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type keyQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// reserveTaskKey returns a key for the queue that no task and no alias uses.
// Collisions are rare enough that a bounded retry is the whole mechanism; a
// queue that cannot produce a free key is far past the point where a longer
// suffix, not another attempt, is the answer.
func reserveTaskKey(q keyQuerier, queue string) (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		suffix, err := newTaskKeySuffix()
		if err != nil {
			return "", err
		}
		key := queue + "-" + suffix
		var taken int
		if err := q.QueryRow(`
			SELECT (SELECT COUNT(*) FROM tasks WHERE task_key = ?)
			     + (SELECT COUNT(*) FROM task_key_aliases WHERE old_key = ?)`,
			key, key).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return key, nil
		}
	}
	return "", fmt.Errorf("tasks: no free key for queue %s after 10 attempts", queue)
}
