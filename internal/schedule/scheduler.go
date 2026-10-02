package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/alekzonder/tariboy/internal/bus"
)

// Publisher is the slice of the bus the scheduler needs.
type Publisher interface {
	Publish(bus.Message) (bus.Message, error)
}

type guardedPublisher interface {
	PublishWithGuard(bus.Message, func(*sql.Tx, time.Time) error) (bus.Message, error)
}

type Scheduler struct {
	store *Store
	pub   Publisher
	log   *slog.Logger
	clock func() time.Time
	after func(time.Duration) <-chan time.Time
}

func NewScheduler(st *Store, pub Publisher, log *slog.Logger,
	clock func() time.Time, after func(time.Duration) <-chan time.Time) *Scheduler {
	if clock == nil {
		clock = time.Now
	}
	if after == nil {
		after = time.After
	}
	return &Scheduler{store: st, pub: pub, log: log, clock: clock, after: after}
}

// Run fires due schedules once per second until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.after(time.Second):
			if _, err := s.fireDue(s.clock()); err != nil {
				s.log.Error("scheduler tick", "err", err)
			}
		}
	}
}

// fireDue publishes every schedule due at/before now and advances/disables each.
func (s *Scheduler) fireDue(now time.Time) (int, error) {
	due, err := s.store.DueBefore(now)
	if err != nil {
		return 0, err
	}
	fired := 0
	for _, sch := range due {
		msg := renderTemplate(sch)
		var publishErr error
		if sch.Channel != "" {
			guarded, ok := s.pub.(guardedPublisher)
			if !ok {
				publishErr = fmt.Errorf("scheduled channel publisher does not support atomic guards")
			} else {
				_, publishErr = guarded.PublishWithGuard(msg, scheduleGuard(sch))
			}
		} else {
			_, publishErr = s.pub.Publish(msg)
		}
		if publishErr != nil {
			if publishErr != bus.ErrPublishGuardDenied {
				s.log.Error("schedule publish", "id", sch.ID, "err", publishErr)
			}
			continue
		}
		fired++
		s.advance(&sch, now)
		if err := s.store.MarkFired(sch); err != nil {
			s.log.Error("schedule mark fired", "id", sch.ID, "err", err)
		}
	}
	return fired, nil
}

func scheduleGuard(sch Schedule) func(*sql.Tx, time.Time) error {
	return func(tx *sql.Tx, now time.Time) error {
		var live int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM schedules WHERE id=? AND enabled=1 AND next_fire_at=?`, sch.ID, sch.NextFireAt).Scan(&live); err != nil {
			return err
		}
		if live == 0 {
			return bus.ErrPublishGuardDenied
		}
		return nil
	}
}

// advance recomputes a cron schedule's next firing; a oneshot is disabled.
func (s *Scheduler) advance(sch *Schedule, now time.Time) {
	if sch.Kind == "oneshot" {
		sch.Enabled = false
		return
	}
	if next, ok := NextAfter(sch.Spec, now); ok {
		sch.NextFireAt = next
		sch.Enabled = true
	} else {
		sch.Enabled = false
	}
}

func renderTemplate(sch Schedule) bus.Message {
	var tpl struct {
		Type    string         `json:"type"`
		Subject map[string]any `json:"subject"`
		Text    string         `json:"text"`
		Data    map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(sch.MessageTemplate), &tpl)
	if tpl.Type == "" {
		tpl.Type = "schedule.fire"
	}
	return bus.Message{
		Channel: sch.Channel, Type: tpl.Type, Subject: tpl.Subject, Text: tpl.Text, Data: tpl.Data,
		Source: "schedule", ProducedByAgent: sch.Agent,
		IdempotencyKey: "schedule:" + sch.ID + ":" + sch.NextFireAt,
	}
}
