package app

import (
	"context"
	"testing"
	"time"
)

// Расписание должно брать ровно то, чьё время подошло: ни сервер с
// выключенным расписанием, ни только что отработавший сервер не должны
// бэкапиться повторно. Первое означало бы бэкап без спроса, второе -
// бесконечный цикл на машине, где RunDue вызывается каждые десять минут.
func TestRunDueRespectsSchedule(t *testing.T) {
	ctx := context.Background()
	a, s, _, _ := setup(t)

	a.Update(func(cfg *Config) error {
		cfg.Servers[0].Schedule = Schedule{Enabled: true, EveryHours: 6}
		return nil
	})

	if err := a.RunDue(ctx, Events{}); err != nil {
		t.Fatalf("первый прогон: %v", err)
	}
	snaps, err := a.Snapshots(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Сервер не бэкапился ни разу, поэтому идёт сразу, не дожидаясь окна.
	if len(snaps) != 1 {
		t.Fatalf("после первого прогона снимков: %d, ожидался 1", len(snaps))
	}

	if err := a.RunDue(ctx, Events{}); err != nil {
		t.Fatalf("второй прогон: %v", err)
	}
	snaps, err = a.Snapshots(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("время ещё не подошло, а снимков стало %d", len(snaps))
	}

	// Отодвигаем последний запуск на семь часов назад - окно в шесть
	// часов пройдено, значит пора.
	a.Update(func(cfg *Config) error {
		cfg.Servers[0].Last.Time = time.Now().Add(-7 * time.Hour)
		return nil
	})
	if err := a.RunDue(ctx, Events{}); err != nil {
		t.Fatalf("третий прогон: %v", err)
	}
	snaps, err = a.Snapshots(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("время подошло, а снимков %d, ожидалось 2", len(snaps))
	}

	// Выключенное расписание не трогается вовсе.
	a.Update(func(cfg *Config) error {
		cfg.Servers[0].Schedule.Enabled = false
		cfg.Servers[0].Last.Time = time.Now().Add(-99 * time.Hour)
		return nil
	})
	if err := a.RunDue(ctx, Events{}); err != nil {
		t.Fatalf("прогон с выключенным расписанием: %v", err)
	}
	snaps, err = a.Snapshots(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("расписание выключено, а снимков стало %d", len(snaps))
	}
}
