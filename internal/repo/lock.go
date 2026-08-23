package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

// Блокировки репозитория.
//
// Без них одновременные операции уничтожают данные, и не теоретически:
// prune считает живыми только те чанки, на которые ссылаются сохранённые
// снимки. Пакеты идущего в этот момент бэкапа не видны никому - снимок
// ещё не записан, - и prune удалит их как мусор. Бэкап после этого
// сохранит снимок, ссылающийся на пустоту.
//
// Двух видов:
//
//	общая       бэкап, восстановление, проверка - их можно сколько угодно
//	исключающая prune, пересборка индекса - только в одиночестве
//
// Взятие устроено как «напиши и осмотрись»: пишем свой файл, читаем все
// остальные, при конфликте убираем свой и отступаем. Два одновременных
// претендента на исключающую блокировку увидят друг друга и оба отступят;
// повторная попытка со случайной задержкой разведёт их. Это дешевле, чем
// требовать от хранилища атомарных операций, которых в S3 нет.
const (
	// lockRefresh - как часто обновляется отметка живости.
	lockRefresh = 45 * time.Second
	// lockStale - после какого молчания блокировка считается брошенной.
	//
	// Втрое больше периода обновления: одна пропущенная запись из-за
	// заминки в сети не должна объявлять живой процесс мёртвым.
	lockStale = 3 * time.Minute
	// lockSettle - пауза между записью своей блокировки и осмотром чужих,
	// чтобы запись успела стать видимой на хранилище со слабой
	// согласованностью листинга.
	lockSettle = 1 * time.Second
)

var ErrLocked = errors.New("autobak: репозиторий занят другой операцией")

type lockFile struct {
	Exclusive bool      `json:"exclusive"`
	Op        string    `json:"op"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	Created   time.Time `json:"created"`
	Refreshed time.Time `json:"refreshed"`
}

func (l lockFile) stale(now time.Time) bool {
	// Отметка из будущего означает расхождение часов между машинами.
	// Считать такую блокировку брошенной опаснее, чем подождать лишнего.
	if l.Refreshed.After(now) {
		return false
	}
	return now.Sub(l.Refreshed) > lockStale
}

func (l lockFile) describe() string {
	return fmt.Sprintf("%s на %s (pid %d, начата %s назад)",
		l.Op, l.Host, l.PID, time.Since(l.Created).Round(time.Second))
}

// Lock - удерживаемая блокировка. Обязательно освобождать через Unlock.
type Lock struct {
	r    *Repo
	name string
	info lockFile

	stop      chan struct{}
	stopped   sync.Once
	refresher sync.WaitGroup

	mu            sync.Mutex
	lastRefreshOK time.Time
}

// Lost сообщает, что блокировка могла протухнуть: отметка живости не
// обновлялась дольше lockStale (сеть отвалилась посреди долгой операции).
//
// Проверяется перед необратимым шагом - сохранением снимка и удалением в
// prune. Пока наша отметка протухла, другой процесс считает репозиторий
// свободным: очистка могла снести уже залитые паки бэкапа, а параллельный
// бэкап - начаться во время prune. Коммитить в таком состоянии - значит
// сохранить снимок, ссылающийся на возможно удалённые чанки.
func (l *Lock) Lost() bool {
	if l == nil || l.r == nil {
		return false // блокировки нет (только чтение) - и терять нечего
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.lastRefreshOK) > lockStale
}

// Lock занимает репозиторий под операцию.
//
// exclusive=true не пускает никого; exclusive=false уживается с другими
// общими, но ждёт исключающую.
func (r *Repo) Lock(ctx context.Context, op string, exclusive bool) (*Lock, error) {
	if !r.be.Caps().CanWrite {
		// Читателю без права записи блокировку взять негде. Это не ошибка:
		// с открытого только на чтение хранилища ничего разрушить нельзя.
		return &Lock{}, nil
	}
	host, _ := os.Hostname()
	id, err := randHex(8)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	l := &Lock{
		r:    r,
		name: DirLocks + "/" + id,
		info: lockFile{
			Exclusive: exclusive, Op: op, Host: host, PID: os.Getpid(),
			Created: now, Refreshed: now,
		},
		stop:          make(chan struct{}),
		lastRefreshOK: now,
	}

	if err := l.write(ctx); err != nil {
		return nil, err
	}

	// Пауза перед «осмотром» - на случай хранилища со слабой
	// согласованностью листинга (часть S3-совместимых, B2): без неё два
	// клиента, записавшие блокировки почти одновременно, могли бы не
	// увидеть друг друга и оба решить, что путь свободен. Для строго
	// согласованного хранилища (локальный диск) она не нужна и
	// пропускается, чтобы не замедлять операции на ровном месте.
	if sc, ok := r.be.(interface{ StronglyConsistent() bool }); !ok || !sc.StronglyConsistent() {
		select {
		case <-time.After(lockSettle):
		case <-ctx.Done():
			l.remove(ctx)
			return nil, ctx.Err()
		}
	}

	if conflict, err := l.conflicts(ctx); err != nil {
		l.remove(ctx)
		return nil, err
	} else if conflict != "" {
		l.remove(ctx)
		return nil, fmt.Errorf("%w: %s", ErrLocked, conflict)
	}

	l.refresher.Add(1)
	go l.keepAlive()
	return l, nil
}

// LockWithRetry ждёт освобождения репозитория.
//
// Нужен расписанию: бэкап, не запустившийся потому, что в этот момент
// шла очистка, - это пропущенный бэкап, а ждать здесь обычно недолго.
func (r *Repo) LockWithRetry(ctx context.Context, op string, exclusive bool, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	attempt := 0
	for {
		l, err := r.Lock(ctx, op, exclusive)
		if err == nil || !errors.Is(err, ErrLocked) {
			return l, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		attempt++
		// Случайная составляющая разводит двух претендентов, которые
		// иначе бесконечно уступали бы друг другу в такт.
		pause := time.Duration(attempt) * time.Second
		if pause > 15*time.Second {
			pause = 15 * time.Second
		}
		// До 500 мс настоящего разброса. Прежний вариант брал длину
		// hex-строки (всегда 2), то есть паузу, одинаковую у обоих
		// претендентов, - и разводил их не лучше, чем без джиттера вовсе.
		pause += time.Duration(rand.IntN(500)) * time.Millisecond
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (l *Lock) write(ctx context.Context) error {
	raw, err := json.Marshal(l.info)
	if err != nil {
		return err
	}
	sealed, err := l.r.key.SealMeta(l.name, raw)
	if err != nil {
		return err
	}
	return l.r.be.Put(ctx, l.name, bytes.NewReader(sealed), int64(len(sealed)))
}

// conflicts возвращает описание мешающей блокировки или пустую строку.
func (l *Lock) conflicts(ctx context.Context) (string, error) {
	names, err := listNames(ctx, l.r.be, DirLocks+"/")
	if err != nil {
		return "", err
	}
	now := time.Now()
	for _, name := range names {
		if name == l.name {
			continue
		}
		other, err := l.r.readLock(ctx, name)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) || errors.Is(err, errLockGarbage) {
				// Блокировка исчезла между листингом и чтением, либо это
				// чужой мусор - ни то, ни другое не наша живая блокировка.
				continue
			}
			// Сетевая ошибка: определить, нет ли конфликта, мы не смогли.
			// Молча проигнорировать - значит, возможно, взять исключающую
			// блокировку поверх живой чужой. Лучше отступить и повторить.
			return "", fmt.Errorf("autobak: не проверить блокировку %s: %w", name, err)
		}
		if other.stale(now) {
			continue
		}
		if l.info.Exclusive || other.Exclusive {
			return other.describe(), nil
		}
	}
	return "", nil
}

// errLockGarbage - файл блокировки не расшифровался или не разобрался.
// Отличается от сетевой ошибки намеренно: мусор можно игнорировать и
// удалять, а вот блокировку, которую мы лишь не смогли прочитать из-за
// сбоя сети, трогать нельзя - за ней может стоять живая операция.
var errLockGarbage = errors.New("autobak: файл блокировки не читается")

func (r *Repo) readLock(ctx context.Context, name string) (lockFile, error) {
	var lf lockFile
	raw, err := backend.ReadAll(ctx, r.be, name, 64*KiB)
	if err != nil {
		return lf, err // сетевая ошибка или ErrNotFound - пробрасываем как есть
	}
	plain, err := r.key.OpenMeta(name, raw)
	if err != nil {
		return lf, fmt.Errorf("%w: %v", errLockGarbage, err)
	}
	if err := json.Unmarshal(plain, &lf); err != nil {
		return lf, fmt.Errorf("%w: %v", errLockGarbage, err)
	}
	return lf, nil
}

// keepAlive обновляет отметку живости, пока операция идёт.
//
// Иначе долгий бэкап был бы объявлен брошенным и очистка снесла бы его
// данные ровно посреди работы.
func (l *Lock) keepAlive() {
	defer l.refresher.Done()
	t := time.NewTicker(lockRefresh)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			// Свой контекст: отмена операции не должна мешать корректно
			// дожить до Unlock.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			l.info.Refreshed = time.Now()
			err := l.write(ctx)
			cancel()
			if err == nil {
				// Отметку об успешном обновлении держим отдельно от
				// info.Refreshed: по ней Lost() понимает, что связь с
				// хранилищем ещё жива и блокировку никто не считает
				// брошенной.
				l.mu.Lock()
				l.lastRefreshOK = time.Now()
				l.mu.Unlock()
			}
		}
	}
}

func (l *Lock) remove(ctx context.Context) {
	if l.r == nil || l.name == "" {
		return
	}
	// Право удаления есть не у всех: агент на сервере его намеренно лишён.
	// Его блокировка просто протухнет через lockStale, и это правильный
	// размен - лишние три минуты ожидания против возможности стереть
	// чужую блокировку с захваченного сервера.
	if l.r.be.Caps().CanDelete {
		_ = l.r.be.Delete(ctx, l.name)
	}
}

// Unlock освобождает репозиторий. Безопасен для nil и повторных вызовов.
func (l *Lock) Unlock() {
	if l == nil || l.r == nil {
		return
	}
	l.stopped.Do(func() { close(l.stop) })
	l.refresher.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l.remove(ctx)
}

// LockInfo - что показать человеку, когда репозиторий занят.
type LockInfo struct {
	Op        string    `json:"op"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	Exclusive bool      `json:"exclusive"`
	Created   time.Time `json:"created"`
	Stale     bool      `json:"stale"`
}

// Locks перечисляет занятые блокировки. Нужен интерфейсу, чтобы вместо
// «занято» показать, кем и с каких пор.
func (r *Repo) Locks(ctx context.Context) ([]LockInfo, error) {
	names, err := listNames(ctx, r.be, DirLocks+"/")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []LockInfo
	for _, name := range names {
		lf, err := r.readLock(ctx, name)
		if err != nil {
			continue
		}
		out = append(out, LockInfo{
			Op: lf.Op, Host: lf.Host, PID: lf.PID, Exclusive: lf.Exclusive,
			Created: lf.Created, Stale: lf.stale(now),
		})
	}
	return out, nil
}

// UnlockStale убирает блокировки, которые никто не обновляет.
//
// Такие остаются после аварийного завершения и после агента, у которого
// нет права удаления. Живые блокировки не трогаются никогда: снять
// работающую - значит вернуть ровно ту гонку, ради которой всё это.
func (r *Repo) UnlockStale(ctx context.Context) (int, error) {
	if !r.be.Caps().CanDelete {
		return 0, backend.ErrNoDelete
	}
	names, err := listNames(ctx, r.be, DirLocks+"/")
	if err != nil {
		return 0, err
	}
	now := time.Now()
	removed := 0
	for _, name := range names {
		lf, err := r.readLock(ctx, name)
		if err != nil {
			// Сносим только заведомо мусорную блокировку (не расшифровалась
			// или не разобралась) - её всё равно никто не сможет обновить.
			// Транзиентную ошибку чтения (сеть, 5xx) не трогаем: за ней
			// может стоять живая операция, которую мы лишь не дочитали, -
			// удалить её значило бы вернуть ту самую гонку.
			if errors.Is(err, errLockGarbage) {
				if err := r.be.Delete(ctx, name); err == nil {
					removed++
				}
			}
			continue
		}
		if !lf.stale(now) {
			continue
		}
		if err := r.be.Delete(ctx, name); err == nil {
			removed++
		}
	}
	return removed, nil
}
