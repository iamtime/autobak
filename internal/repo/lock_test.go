package repo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

func TestLockExcludesExclusive(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	// Общие уживаются друг с другом.
	a, err := r.Lock(ctx, "бэкап A", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Lock(ctx, "бэкап B", false)
	if err != nil {
		t.Fatalf("две общие блокировки не ужились: %v", err)
	}

	// Исключающая при живых общих не берётся.
	if _, err := r.Lock(ctx, "очистка", true); !errors.Is(err, ErrLocked) {
		t.Fatalf("очистка стартовала при идущих бэкапах: %v", err)
	}
	a.Unlock()
	if _, err := r.Lock(ctx, "очистка", true); !errors.Is(err, ErrLocked) {
		t.Fatal("очистка стартовала при одном оставшемся бэкапе")
	}
	b.Unlock()

	// Освободилось - берётся.
	ex, err := r.Lock(ctx, "очистка", true)
	if err != nil {
		t.Fatalf("после освобождения очистка не стартовала: %v", err)
	}
	// И теперь не пускает никого.
	if _, err := r.Lock(ctx, "бэкап C", false); !errors.Is(err, ErrLocked) {
		t.Fatalf("бэкап стартовал во время очистки: %v", err)
	}
	ex.Unlock()

	if _, err := r.Lock(ctx, "бэкап D", false); err != nil {
		t.Fatalf("после очистки бэкап не стартовал: %v", err)
	}
}

// Ровно та гонка, ради которой всё: очистка не должна начаться, пока
// идёт бэкап, иначе она удалит его паки как ничейные.
func TestPruneRefusesDuringBackup(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)
	writeSnapshot(t, r, "s1", "prod", time.Now().AddDate(0, 0, -5), realisticData(1, 4*MiB))
	writeSnapshot(t, r, "s2", "prod", time.Now(), realisticData(2, 4*MiB))

	backup, err := r.Lock(ctx, "бэкап prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Unlock()

	opt := DefaultPruneOptions()
	opt.Policy = Retention{Last: 1}
	// Ждать долго незачем: бэкап держим сами и отпускать не собираемся.
	opt.LockWait = 2 * time.Second
	start := time.Now()
	_, err = r.Prune(ctx, opt)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("очистка пошла во время бэкапа: %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("очистка не пыталась дождаться, отказала мгновенно (%s)", waited)
	}
	t.Logf("очистка отклонена: %v", err)
}

// Брошенная блокировка не должна держать репозиторий вечно: процесс мог
// умереть, а у агента вовсе нет права её удалить.
func TestStaleLockIsIgnoredAndCleanable(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	l, err := r.Lock(ctx, "умерший бэкап", true)
	if err != nil {
		t.Fatal(err)
	}
	// Останавливаем обновление и отматываем отметку в прошлое -
	// ровно то, что видно снаружи после аварийного завершения.
	l.stopped.Do(func() { close(l.stop) })
	l.refresher.Wait()
	l.info.Refreshed = time.Now().Add(-2 * lockStale)
	if err := l.write(ctx); err != nil {
		t.Fatal(err)
	}

	locks, err := r.Locks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || !locks[0].Stale {
		t.Fatalf("брошенная блокировка не опознана: %+v", locks)
	}

	// Её наличие не мешает работать.
	fresh, err := r.Lock(ctx, "новый бэкап", true)
	if err != nil {
		t.Fatalf("брошенная блокировка помешала работе: %v", err)
	}
	fresh.Unlock()

	n, err := r.UnlockStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("убрано %d брошенных блокировок вместо 1", n)
	}
}

// Живую блокировку снимать нельзя: это вернуло бы ровно ту гонку,
// ради которой всё затевалось.
func TestUnlockStaleKeepsLiveLocks(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	live, err := r.Lock(ctx, "идущий бэкап", false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Unlock()

	n, err := r.UnlockStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("снято %d живых блокировок", n)
	}
	if _, err := r.Lock(ctx, "очистка", true); !errors.Is(err, ErrLocked) {
		t.Fatal("после уборки живая блокировка перестала защищать")
	}
}

// Гонка за исключающей блокировкой: из нескольких претендентов должен
// пройти ровно один.
func TestExclusiveLockRaceHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	const claimants = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []*Lock

	for range claimants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := r.Lock(ctx, "очистка", true)
			if err != nil {
				return
			}
			mu.Lock()
			won = append(won, l)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Ноль победителей допустим: все увидели друг друга и отступили,
	// повторная попытка их разведёт. Двое победителей - недопустимо.
	if len(won) > 1 {
		t.Fatalf("исключающую блокировку взяли %d процессов одновременно", len(won))
	}
	t.Logf("из %d претендентов прошло: %d", claimants, len(won))
	for _, l := range won {
		l.Unlock()
	}
}

// Хранилище только для чтения не должно требовать блокировок: писать
// в него всё равно нечем, а взять блокировку негде.
func TestLockOnReadOnlyBackend(t *testing.T) {
	ctx := context.Background()
	r, _, dir := testRepo(t)
	_ = r

	ro, err := backend.OpenLocal(dir, backend.Caps{})
	if err != nil {
		t.Skip("каталог недоступен только на чтение")
	}
	rr, err := Open(ctx, ro, "пароль")
	if err != nil {
		t.Fatal(err)
	}
	l, err := rr.Lock(ctx, "проверка", false)
	if err != nil {
		t.Fatalf("чтение потребовало блокировки: %v", err)
	}
	l.Unlock()
}
