package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

// --- Сборка мусора --------------------------------------------------------

type PruneOptions struct {
	Policy Retention
	// Server ограничивает очистку снимками одного сервера. Пусто - весь
	// репозиторий по одной политике.
	//
	// Это защита от потери данных в общем хранилище: несколько серверов
	// пишут в один репозиторий, у каждого своя политика хранения, и
	// очистка сервера A не должна применять политику A к истории сервера
	// B. Снимки других серверов при этом целиком остаются живыми - вместе
	// со всеми их чанками.
	Server string
	// DryRun считает всё, но ничего не трогает. Интерфейс обязан сначала
	// показать результат dry-run и только потом спрашивать подтверждение:
	// удаление снимков необратимо.
	DryRun bool
	// RepackWaste - доля мёртвых данных в паке, начиная с которой пак
	// переписывается. 0.25 - компромисс: ниже начинается бесконечная
	// перезапись почти живых паков, выше в репозитории копится мусор.
	RepackWaste float64
	// LockWait - сколько ждать освобождения репозитория. Ноль означает
	// значение по умолчанию: очистка обычно запускается по расписанию
	// сразу после бэкапов, и подождать чужой бэкап дешевле, чем
	// пропустить очистку до завтра.
	LockWait time.Duration
	Progress func(stage string, done, total int)
}

func DefaultPruneOptions() PruneOptions {
	return PruneOptions{Policy: DefaultRetention(), RepackWaste: 0.25, LockWait: 2 * time.Minute}
}

type PruneReport struct {
	SnapshotsKept    int      `json:"snapshots_kept"`
	SnapshotsRemoved []string `json:"snapshots_removed"`
	PacksTotal       int      `json:"packs_total"`
	PacksDeleted     int      `json:"packs_deleted"`
	PacksRepacked    int      `json:"packs_repacked"`
	ChunksAlive      int      `json:"chunks_alive"`
	ChunksDead       int      `json:"chunks_dead"`
	BytesFreed       int64    `json:"bytes_freed"`
	DryRun           bool     `json:"dry_run"`
}

func (r *PruneReport) Summary() string {
	verb := "освободит"
	if !r.DryRun {
		verb = "освобождено"
	}
	return fmt.Sprintf("снимков удалить: %d (останется %d); паков: -%d, переписано %d; %s %s",
		len(r.SnapshotsRemoved), r.SnapshotsKept, r.PacksDeleted, r.PacksRepacked,
		verb, HumanBytes(r.BytesFreed))
}

// Prune применяет политику хранения и удаляет то, на что больше никто
// не ссылается.
//
// Порядок операций выбран так, чтобы обрыв на любом шаге не разрушил
// репозиторий: сначала пишется всё новое, затем переключается индекс,
// и только в самом конце удаляется старое. Прерванный prune оставляет
// лишние объекты, но никогда - недостающие.
func (r *Repo) Prune(ctx context.Context, opt PruneOptions) (*PruneReport, error) {
	if !opt.DryRun && !r.be.Caps().CanDelete {
		return nil, backend.ErrNoDelete
	}
	progress := opt.Progress
	if progress == nil {
		progress = func(string, int, int) {}
	}
	rep := &PruneReport{DryRun: opt.DryRun}

	// Исключающая блокировка. Сухой прогон ничего не удаляет, но и он
	// считает по списку снимков - а решение, принятое по устаревшему
	// списку, человек увидит как «будет удалено» и подтвердит.
	wait := opt.LockWait
	if wait == 0 {
		wait = 2 * time.Minute
	}
	lock, err := r.LockWithRetry(ctx, "очистка старых снимков", true, wait)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	// Индекс перечитывается уже под блокировкой. Иначе prune работал бы по
	// снимку индекса из памяти, снятому до захвата: другой процесс мог
	// успеть переписать паки и индексы, и мы удалили бы паки, на которые
	// ссылается устаревшая карта, а живые - потеряли.
	r.idx = NewIndex()
	if err := loadAllIndexes(ctx, r.be, r.key, r.idx); err != nil {
		return nil, err
	}

	// Строгий листинг: нечитаемый манифест здесь - это стоп, а не пропуск.
	// Пропущенный снимок prune счёл бы несуществующим и удалил бы его
	// чанки; для очистки, в отличие от показа списка, это потеря данных.
	snaps, err := r.listSnapshotsStrict(ctx)
	if err != nil {
		return nil, err
	}

	// Разделяем снимки на «этого сервера» и «чужие». Политика применяется
	// только к своим; чужие целиком остаются живыми, чтобы очистка одного
	// сервера не тронула историю другого в общем репозитории.
	mine, others := snaps, []*Snapshot(nil)
	if opt.Server != "" {
		mine, others = nil, nil
		for _, s := range snaps {
			if s.Server == opt.Server {
				mine = append(mine, s)
			} else {
				others = append(others, s)
			}
		}
	}
	keepMine, remove := opt.Policy.Apply(mine, time.Now())
	if len(keepMine) == 0 && len(mine) > 0 {
		return nil, ErrWouldDeleteAll
	}
	keep := append(keepMine, others...)
	rep.SnapshotsKept = len(keep)
	for _, s := range remove {
		rep.SnapshotsRemoved = append(rep.SnapshotsRemoved, s.ID)
	}

	// Живые чанки собираются по оставляемым снимкам. Дерево каждого снимка
	// тоже лежит в репозитории чанками, поэтому считается вместе с данными.
	alive := map[ChunkID]struct{}{}
	for i, s := range keep {
		progress("чтение снимков", i, len(keep))
		for _, id := range s.Tree {
			alive[id] = struct{}{}
		}
		if err := r.ReadTree(ctx, s, func(n *Node) error {
			for _, id := range n.Chunks {
				alive[id] = struct{}{}
			}
			return nil
		}); err != nil {
			// Снимок, дерево которого не читается, нельзя считать пустым:
			// иначе prune удалил бы данные, на которые он ссылается.
			return nil, fmt.Errorf("autobak: снимок %s не читается, prune остановлен: %w", s.ID, err)
		}
	}
	rep.ChunksAlive = len(alive)

	type packState struct {
		live, dead    []blobEntry
		liveB, totalB int64
	}
	packs := map[string]*packState{}
	r.idx.Each(func(id ChunkID, packID string, loc Location) bool {
		ps := packs[packID]
		if ps == nil {
			ps = &packState{}
			packs[packID] = ps
		}
		e := blobEntry{ID: id, Off: loc.Off, Len: loc.Len, PLen: loc.PLen, Comp: loc.Comp}
		ps.totalB += int64(loc.Len)
		if _, ok := alive[id]; ok {
			ps.live = append(ps.live, e)
			ps.liveB += int64(loc.Len)
		} else {
			ps.dead = append(ps.dead, e)
			rep.ChunksDead++
		}
		return true
	})
	rep.PacksTotal = len(packs)

	var toDelete []string
	var toRepack []string
	survivors := []indexPack{}
	for id, ps := range packs {
		switch {
		case len(ps.live) == 0:
			toDelete = append(toDelete, id)
			rep.BytesFreed += ps.totalB
		case len(ps.dead) > 0 && float64(ps.totalB-ps.liveB)/float64(ps.totalB) >= opt.RepackWaste:
			toRepack = append(toRepack, id)
			rep.BytesFreed += ps.totalB - ps.liveB
		default:
			survivors = append(survivors, indexPack{ID: id, Blobs: ps.live})
		}
	}
	rep.PacksDeleted = len(toDelete)
	rep.PacksRepacked = len(toRepack)

	if opt.DryRun {
		return rep, nil
	}

	// Перед разрушающей фазой проверяем, что исключающая блокировка всё
	// ещё за нами. Если связь с хранилищем прерывалась дольше lockStale,
	// другой процесс мог счесть репозиторий свободным и начать бэкап -
	// удалять что-либо в этот момент нельзя.
	if lock.Lost() {
		return nil, errors.New("autobak: связь с хранилищем прерывалась дольше допустимого, " +
			"блокировка могла протухнуть - очистка остановлена перед удалением")
	}

	// Список старых индексов снимается до записи нового, иначе новый попадёт
	// в него сам и будет удалён вместе со старыми.
	oldIndexes, err := listNames(ctx, r.be, DirIndex+"/")
	if err != nil {
		return nil, err
	}

	// 1. Переписываем частично живые паки в новые.
	for i, id := range toRepack {
		progress("уплотнение паков", i, len(toRepack))
		fresh, err := r.repackPack(ctx, id, packs[id].live)
		if err != nil {
			return nil, err
		}
		survivors = append(survivors, fresh...)
	}

	// 2. Новый сводный индекс. Пишется до удаления чего бы то ни было.
	if _, err := saveIndex(ctx, r.be, r.key, survivors); err != nil {
		return nil, err
	}

	// 3. Старые индексы больше не нужны - в них ссылки на удаляемые паки.
	for _, name := range oldIndexes {
		if err := r.be.Delete(ctx, name); err != nil {
			return nil, err
		}
	}

	// 4. Снимки и паки.
	for _, s := range remove {
		if err := r.DeleteSnapshot(ctx, s.ID); err != nil {
			return nil, err
		}
	}
	for i, id := range append(toDelete, toRepack...) {
		progress("удаление паков", i, len(toDelete)+len(toRepack))
		if err := r.be.Delete(ctx, packName(id)); err != nil {
			return nil, err
		}
	}

	// Индекс в памяти теперь не соответствует хранилищу - перечитываем.
	r.idx = NewIndex()
	if err := loadAllIndexes(ctx, r.be, r.key, r.idx); err != nil {
		return nil, err
	}
	r.cacheMu.Lock()
	r.cacheID, r.cacheB = "", nil
	r.cacheMu.Unlock()
	return rep, nil
}

// repackPack переносит живые чанки старого пака в новые паки.
func (r *Repo) repackPack(ctx context.Context, oldID string, live []blobEntry) ([]indexPack, error) {
	var out []indexPack
	pb, err := newPackBuilder()
	if err != nil {
		return nil, err
	}
	flush := func() error {
		if pb.count() == 0 {
			return nil
		}
		data, err := pb.finish(r.key)
		if err != nil {
			return err
		}
		if err := r.be.PutNew(ctx, packName(pb.id), bytes.NewReader(data), int64(len(data))); err != nil &&
			!errors.Is(err, backend.ErrExists) {
			return err
		}
		out = append(out, indexPack{ID: pb.id, Blobs: pb.entries})
		pb, err = newPackBuilder()
		return err
	}

	for _, e := range live {
		// Читаем через LoadChunk: он заодно проверяет подпись, так что
		// уплотнение не может незаметно перенести испорченный чанк.
		plain, err := r.LoadChunk(ctx, e.ID)
		if err != nil {
			return nil, fmt.Errorf("autobak: уплотнение пака %s: %w", oldID, err)
		}
		if _, err := pb.add(r.key, e.ID, plain); err != nil {
			return nil, err
		}
		if pb.size() >= packTargetSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func listNames(ctx context.Context, be backend.Backend, prefix string) ([]string, error) {
	var out []string
	err := be.List(ctx, prefix, func(fi backend.FileInfo) error {
		out = append(out, fi.Name)
		return nil
	})
	return out, err
}

// --- Проверка целостности -------------------------------------------------

type VerifyOptions struct {
	// Sample - доля чанков, которые действительно читаются и расшифровываются.
	// 1.0 - полная проверка. Меньшая доля превращает проверку в выборочную:
	// на репозитории в терабайт полная занимает часы, а выборочная в 5%
	// ловит деградацию хранилища за минуты.
	Sample float64
	// Snapshots ограничивает проверку конкретными снимками (пусто - все).
	Snapshots []string
	Progress  func(done, total int)
}

type VerifyReport struct {
	SnapshotsChecked int      `json:"snapshots_checked"`
	TreesBad         int      `json:"trees_bad"`
	ChunksReferenced int      `json:"chunks_referenced"`
	ChunksMissing    int      `json:"chunks_missing"`
	ChunksRead       int      `json:"chunks_read"`
	ChunksBad        int      `json:"chunks_bad"`
	Problems         []string `json:"problems,omitempty"`
	Duration         Duration `json:"duration"`
}

// OK намеренно требует и пустого списка проблем: снимок, дерево которого
// не читается, формально не теряет ни одного чанка, но восстановить из него
// уже нельзя ничего. Считать такой репозиторий исправным недопустимо.
func (v *VerifyReport) OK() bool {
	return v.ChunksMissing == 0 && v.ChunksBad == 0 && v.TreesBad == 0 && len(v.Problems) == 0
}

func (v *VerifyReport) Summary() string {
	if v.OK() {
		return fmt.Sprintf("проверено снимков: %d, чанков: %d (прочитано %d) - всё цело",
			v.SnapshotsChecked, v.ChunksReferenced, v.ChunksRead)
	}
	return fmt.Sprintf("ПРОБЛЕМЫ: нечитаемых снимков %d, потеряно чанков %d, повреждено %d из %d",
		v.TreesBad, v.ChunksMissing, v.ChunksBad, v.ChunksReferenced)
}

// Verify проверяет, что снимки полны и данные читаются.
//
// Две разные вещи проверяются раздельно: что все чанки снимка вообще есть
// в репозитории (быстро, по индексу) и что они физически читаются и проходят
// проверку подписи (медленно, выборочно). Бэкап, который никто ни разу не
// прочитал, бэкапом считать нельзя.
func (r *Repo) Verify(ctx context.Context, opt VerifyOptions) (*VerifyReport, error) {
	start := time.Now()
	rep := &VerifyReport{}

	// Общая: проверка только читает, но очистка посреди неё удалила бы
	// проверяемое, и мы отчитались бы о потерянных чанках, которых на
	// момент начала не было.
	lock, err := r.LockWithRetry(ctx, "проверка целостности", false, time.Minute)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	progress := opt.Progress
	if progress == nil {
		progress = func(int, int) {}
	}

	// Нечитаемый манифест здесь - это проблема, а не пустое место:
	// молча пропустив его (как делает ListSnapshots для интерфейса),
	// verify отчитался бы «всё цело» о снимке, который не восстановить.
	snaps, failed, err := r.loadSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range failed {
		rep.TreesBad++
		rep.Problems = append(rep.Problems, "снимок "+id+": манифест не читается")
	}
	if len(opt.Snapshots) > 0 {
		want := map[string]bool{}
		for _, id := range opt.Snapshots {
			want[id] = true
		}
		var filtered []*Snapshot
		for _, s := range snaps {
			if want[s.ID] {
				filtered = append(filtered, s)
				delete(want, s.ID)
			}
		}
		// Запрошенный, но не найденный снимок - тоже проблема: иначе
		// проверка удалённого снимка отрапортовала бы SnapshotsChecked=0
		// и «всё хорошо».
		for id := range want {
			rep.Problems = append(rep.Problems, "снимок "+id+" не найден в репозитории")
		}
		snaps = filtered
	}

	refs := map[ChunkID]struct{}{}
	for _, s := range snaps {
		rep.SnapshotsChecked++
		for _, id := range s.Tree {
			refs[id] = struct{}{}
		}
		if err := r.ReadTree(ctx, s, func(n *Node) error {
			for _, id := range n.Chunks {
				refs[id] = struct{}{}
			}
			return nil
		}); err != nil {
			rep.TreesBad++
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("снимок %s: дерево не читается: %v", s.ID, err))
			continue
		}
	}
	rep.ChunksReferenced = len(refs)

	var toRead []ChunkID
	neededPacks := map[string]struct{}{}
	rnd := rand.New(rand.NewPCG(uint64(start.UnixNano()), 0x9E3779B9))
	for id := range refs {
		if !r.idx.Has(id) {
			rep.ChunksMissing++
			if len(rep.Problems) < 100 {
				rep.Problems = append(rep.Problems, "чанк потерян: "+id.String()[:16])
			}
			continue
		}
		if p, _, ok := r.idx.Lookup(id); ok {
			neededPacks[p] = struct{}{}
		}
		if opt.Sample >= 1 || rnd.Float64() < opt.Sample {
			toRead = append(toRead, id)
		}
	}

	// Проверяем, что все нужные паки физически на месте. Выборочное чтение
	// чанков при малом Sample легко промахивается мимо целиком пропавшего
	// пака (провайдер потерял объект, админ удалил руками), а дешёвый
	// листинг data/ ловит это сразу и полностью.
	present := map[string]struct{}{}
	if err := r.be.List(ctx, DirData+"/", func(fi backend.FileInfo) error {
		parts := strings.Split(fi.Name, "/")
		if len(parts) == 3 {
			present[parts[2]] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for p := range neededPacks {
		if _, ok := present[p]; !ok {
			rep.ChunksMissing++ // считаем как потерю данных для OK()
			if len(rep.Problems) < 100 {
				rep.Problems = append(rep.Problems, "пак отсутствует в хранилище: "+p)
			}
		}
	}

	// Чтение идёт по порядку паков: так срабатывает кэш целого пака и
	// проверка не превращается в тысячи случайных запросов к хранилищу.
	sortChunksByPack(r.idx, toRead)

	var mu sync.Mutex
	for i, id := range toRead {
		if i%64 == 0 {
			progress(i, len(toRead))
			if err := ctx.Err(); err != nil {
				return rep, err
			}
		}
		if _, err := r.LoadChunk(ctx, id); err != nil {
			mu.Lock()
			rep.ChunksBad++
			if len(rep.Problems) < 100 {
				rep.Problems = append(rep.Problems, err.Error())
			}
			mu.Unlock()
			continue
		}
		rep.ChunksRead++
	}
	progress(len(toRead), len(toRead))
	rep.Duration = Duration(time.Since(start))
	return rep, nil
}

func sortChunksByPack(idx *Index, ids []ChunkID) {
	type kv struct {
		id   ChunkID
		pack string
		off  uint64
	}
	tmp := make([]kv, len(ids))
	for i, id := range ids {
		p, loc, _ := idx.Lookup(id)
		tmp[i] = kv{id, p, loc.Off}
	}
	// Вставками: список обычно уже почти упорядочен, потому что чанки
	// добавлялись в паки последовательно.
	for i := 1; i < len(tmp); i++ {
		for j := i; j > 0 && (tmp[j].pack < tmp[j-1].pack ||
			(tmp[j].pack == tmp[j-1].pack && tmp[j].off < tmp[j-1].off)); j-- {
			tmp[j], tmp[j-1] = tmp[j-1], tmp[j]
		}
	}
	for i, x := range tmp {
		ids[i] = x.id
	}
}

// --- Восстановление индекса -----------------------------------------------

// RebuildIndex собирает индекс заново, читая хвосты всех паков.
//
// Нужен, если каталог index/ потерян, повреждён или расходится с data/ -
// например, после прерванного бэкапа, успевшего залить паки, но не индекс.
// Данные при этом целы: каждый пак самоописывающийся.
func (r *Repo) RebuildIndex(ctx context.Context, progress func(done, total int)) (int, error) {
	if progress == nil {
		progress = func(int, int) {}
	}
	// Исключающая: пересборка переписывает индекс целиком, и параллельный
	// бэкап дописал бы в него свои паки уже после того, как мы прочитали
	// список - они бы потерялись.
	lock, err := r.LockWithRetry(ctx, "пересборка индекса", true, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	defer lock.Unlock()
	var packIDs []string
	if err := r.be.List(ctx, DirData+"/", func(fi backend.FileInfo) error {
		parts := strings.Split(fi.Name, "/")
		if len(parts) == 3 {
			packIDs = append(packIDs, parts[2])
		}
		return nil
	}); err != nil {
		return 0, err
	}

	type result struct {
		pack indexPack
		err  error
	}
	results := make([]result, len(packIDs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var done int
	var mu sync.Mutex

	for i, id := range packIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			blobs, err := readPackTrailer(ctx, r.be, r.key, id)
			results[i] = result{indexPack{ID: id, Blobs: blobs}, err}
			mu.Lock()
			done++
			progress(done, len(packIDs))
			mu.Unlock()
		}()
	}
	wg.Wait()

	var packs []indexPack
	var broken []string
	for i, res := range results {
		if res.err != nil {
			broken = append(broken, packIDs[i])
			continue
		}
		packs = append(packs, res.pack)
	}

	// Старые индексы удаляем только после успешной записи нового.
	old, err := listNames(ctx, r.be, DirIndex+"/")
	if err != nil {
		return 0, err
	}
	if _, err := saveIndex(ctx, r.be, r.key, packs); err != nil {
		return 0, err
	}
	// Старые индексы удаляем ТОЛЬКО если все паки прочитались. Если у части
	// паков испорчен трейлер, в старом индексе могли остаться рабочие
	// ссылки на их блобы (по Off/Len, мимо трейлера) - удалив старый
	// индекс, мы потеряли бы к ним доступ навсегда. Пересборка не должна
	// ухудшать читаемость репозитория. Новый индекс уже дописан; старые
	// оставляем как запасную карту, loadAllIndexes сольёт их вместе.
	if r.be.Caps().CanDelete && len(broken) == 0 {
		for _, name := range old {
			if err := r.be.Delete(ctx, name); err != nil {
				return 0, err
			}
		}
	}

	r.idx = NewIndex()
	if err := loadAllIndexes(ctx, r.be, r.key, r.idx); err != nil {
		return 0, err
	}
	if len(broken) > 0 {
		return len(packs), fmt.Errorf("autobak: индекс перестроен, но %d паков нечитаемы: %s",
			len(broken), strings.Join(broken[:min(5, len(broken))], ", "))
	}
	return len(packs), nil
}

// HumanBytes форматирует размер так, как его ожидает увидеть человек.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), [...]string{"КБ", "МБ", "ГБ", "ТБ", "ПБ"}[exp])
}
