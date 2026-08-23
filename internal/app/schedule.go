package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/iamtime/autobak/internal/notify"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
)

// RunDue выполняет всё, чьё время подошло.
//
// Одна и та же процедура вызывается из планировщика на компьютере, из
// cron на сервере и из встроенного таймера веб-интерфейса. Разными их
// делать нельзя: расписание, работающее в одном интерфейсе иначе, чем в
// другом, обнаруживается ровно в тот день, когда бэкап понадобился.
//
// Порядок не случайный: сначала бэкапы, затем очистка, затем зеркала -
// копировать имеет смысл уже свежие данные, - и в конце проверка
// восстановлением. Ошибка на любом сервере не отменяет остальные:
// один недоступный сервер не должен оставлять без бэкапа все прочие.
func (a *App) RunDue(ctx context.Context, ev Events) error {
	now := time.Now()
	var failed []string

	// Из конфигурации забираем только то, что нужно для решения, и сразу
	// отпускаем замок: сам бэкап идёт часами, а держать общую блокировку
	// всё это время значило бы подвесить интерфейс.
	type dueServer struct {
		id, name string
	}
	var queue []dueServer
	a.Read(func(cfg *Config) {
		for _, s := range cfg.Servers {
			if !s.Schedule.Enabled {
				continue
			}
			// Сервер, который не бэкапился ни разу, берётся сразу: ждать
			// следующего окна значит оставить его без копии на сутки
			// после настройки.
			due := s.Schedule.Next(s.Last.Time, s.Last.Time)
			if !s.Last.Time.IsZero() && due.After(now) {
				continue
			}
			queue = append(queue, dueServer{s.ID, s.Name})
		}
	})

	for _, s := range queue {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev.log("info", "бэкап по расписанию: "+s.name)
		if _, err := a.Backup(ctx, s.id, ev); err != nil {
			ev.log("error", fmt.Sprintf("%s: %v", s.name, err))
			failed = append(failed, s.name)
			continue
		}
		// Очистка сразу после бэкапа, а не в конце: так политика хранения
		// применяется к уже пополненной истории, и место освобождается
		// до того, как в хранилище поедет следующий сервер.
		if _, err := a.Prune(ctx, s.id, false, ev); err != nil {
			ev.log("warn", fmt.Sprintf("%s: очистка не удалась: %v", s.name, err))
			// Молчащая очистка опасна: репозиторий, который постоянно занят
			// чужой машиной, месяцами не чистится, и место кончается
			// незаметно. Уведомляем, а не только пишем в журнал.
			a.notifyAbout(ctx, notify.Message{
				Level: notify.LevelWarning, Server: s.name,
				Title: "очистка по расписанию не выполнена",
				Body:  err.Error(),
			}, ev)
		}
	}

	a.runMirrors(ctx, ev)
	a.runDueDrills(ctx, ev)

	// Проверка «давно не было» делается после прогона, а не вместо него:
	// она ловит серверы, до которых расписание не дошло вовсе - например,
	// потому что машина была выключена несколько дней.
	if stale := a.CheckStale(ctx, ev); len(stale) > 0 {
		ev.log("error", "давно не бэкапились: "+strings.Join(stale, ", "))
		failed = append(failed, stale...)
	}
	if len(failed) > 0 {
		return fmt.Errorf("требуют внимания: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (a *App) runMirrors(ctx context.Context, ev Events) {
	type mirrorPair struct {
		id, name, to string
	}
	var pairs []mirrorPair
	a.Read(func(cfg *Config) {
		for _, cr := range cfg.Repos {
			if cr.MirrorTo == "" {
				continue
			}
			pairs = append(pairs, mirrorPair{cr.ID, cr.Name, cr.MirrorTo})
		}
	})
	for _, cr := range pairs {
		if ctx.Err() != nil {
			return
		}
		ev.log("info", fmt.Sprintf("зеркало: %s → %s", cr.name, cr.to))
		rep, err := a.Mirror(ctx, cr.id, cr.to, repo.DefaultMirrorOptions(), ev)
		if err != nil {
			ev.log("warn", fmt.Sprintf("зеркалирование не выполнено: %v", err))
			// Вторая копия, молча переставшая обновляться, - это иллюзия
			// защиты. О сбое зеркала нужно знать, а не находить его тогда,
			// когда основной репозиторий уже потерян.
			a.notifyAbout(ctx, notify.Message{
				Level: notify.LevelWarning, Server: cr.name,
				Title: "зеркалирование не выполнено",
				Body:  err.Error(),
			}, ev)
			continue
		}
		ev.log("info", rep.Summary())
	}
}

func (a *App) runDueDrills(ctx context.Context, ev Events) {
	for _, repoID := range a.DrillDue() {
		s, ok := a.ServerForRepo(repoID)
		if !ok {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		ev.log("info", "проверка восстановлением: "+s.Name)
		rep, err := a.Drill(ctx, s.ID, "", restore.DrillOptions{MaxBytes: 1 << 30}, ev)
		if err != nil {
			ev.log("warn", fmt.Sprintf("проверка не выполнена: %v", err))
			continue
		}
		ev.log("info", rep.Summary())
	}
}
