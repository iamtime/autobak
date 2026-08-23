package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

var (
	ErrNotInitialized = errors.New("autobak: в этом месте нет репозитория")
	ErrAlreadyExists  = errors.New("autobak: репозиторий здесь уже есть")
	ErrNoKey          = errors.New("autobak: ни один ключ не подошёл к паролю")
)

type Repo struct {
	be  backend.Backend
	key *MasterKey
	cfg Config
	chp ChunkerParams
	idx *Index

	// cache держит последний прочитанный пак целиком. Чанки одного файла
	// почти всегда лежат в одном паке подряд, поэтому при восстановлении
	// это превращает сотни range-запросов в один.
	cacheMu sync.Mutex
	cacheID string
	cacheB  []byte
}

func (r *Repo) Backend() backend.Backend { return r.be }
func (r *Repo) Key() *MasterKey          { return r.key }
func (r *Repo) ID() string               { return r.cfg.ID }
func (r *Repo) Index() *Index            { return r.idx }
func (r *Repo) Chunker() ChunkerParams   { return r.chp }

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("autobak: нет источника случайности: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Init создаёт репозиторий: новый master key, обёрнутый паролем, и config
// со случайным seed чанкера.
//
// Возвращает ещё и recovery-код. Показать его пользователю обязан
// вызывающий: это единственный момент, когда код существует в открытом
// виде, и второго шанса записать его не будет.
func Init(ctx context.Context, be backend.Backend, password, hint string) (*Repo, string, error) {
	if _, err := be.Stat(ctx, FileConfig); err == nil {
		return nil, "", ErrAlreadyExists
	} else if !errors.Is(err, backend.ErrNotFound) {
		return nil, "", err
	}

	key, err := NewMasterKey()
	if err != nil {
		return nil, "", err
	}
	var seedBuf [8]byte
	if _, err := rand.Read(seedBuf[:]); err != nil {
		return nil, "", fmt.Errorf("autobak: нет источника случайности: %w", err)
	}
	chp := DefaultChunkerParams(binary.BigEndian.Uint64(seedBuf[:]))

	sealed, err := key.SealMeta(FileConfig, mustJSON(secretConfig{Chunker: chp}))
	if err != nil {
		return nil, "", err
	}
	id, err := randHex(8)
	if err != nil {
		return nil, "", err
	}
	cfg := Config{Version: RepoVersion, ID: id, Created: time.Now().UTC(), Sealed: sealed}

	// Ключ пишется раньше config: если запись оборвётся между ними, останется
	// осиротевший файл ключа, и Init можно будет просто повторить. В обратном
	// порядке получился бы репозиторий, который нечем открыть.
	kf, err := WrapMasterKey(key, password, hint, DefaultKDFParams(), time.Now())
	if err != nil {
		return nil, "", err
	}
	if err := putJSONNew(ctx, be, DirKeys+"/"+kf.ID, kf); err != nil {
		return nil, "", err
	}
	// config пишется через PutNew: если два процесса инициализируют один
	// бакет одновременно, второй получит ErrExists, а не затрёт config и
	// ключ первого, сделав всё записанное первым паролем нечитаемым.
	if err := putJSONNew(ctx, be, FileConfig, cfg); err != nil {
		if errors.Is(err, backend.ErrExists) {
			return nil, "", ErrAlreadyExists
		}
		return nil, "", err
	}
	return &Repo{be: be, key: key, cfg: cfg, chp: chp, idx: NewIndex()}, key.RecoveryCode(), nil
}

// Open открывает репозиторий по паролю, перебирая файлы ключей.
func Open(ctx context.Context, be backend.Backend, password string) (*Repo, error) {
	cfg, err := readConfig(ctx, be)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := be.List(ctx, DirKeys+"/", func(fi backend.FileInfo) error {
		names = append(names, fi.Name)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, name := range names {
		raw, err := backend.ReadAll(ctx, be, name, maxKeyFileSize)
		if err != nil {
			continue
		}
		var kf KeyFile
		if json.Unmarshal(raw, &kf) != nil {
			continue
		}
		key, err := UnwrapMasterKey(&kf, password)
		if err != nil {
			continue // не тот ключ - пробуем следующий
		}
		return finishOpen(ctx, be, key, cfg)
	}
	return nil, ErrNoKey
}

// OpenWithKey открывает репозиторий master key напрямую: так работает агент
// на сервере (у него ключ в файле, а не пароль в голове) и восстановление
// по recovery-коду.
func OpenWithKey(ctx context.Context, be backend.Backend, key *MasterKey) (*Repo, error) {
	cfg, err := readConfig(ctx, be)
	if err != nil {
		return nil, err
	}
	return finishOpen(ctx, be, key, cfg)
}

func finishOpen(ctx context.Context, be backend.Backend, key *MasterKey, cfg Config) (*Repo, error) {
	plain, err := key.OpenMeta(FileConfig, cfg.Sealed)
	if err != nil {
		return nil, fmt.Errorf("autobak: ключ не подходит к этому репозиторию: %w", err)
	}
	var sc secretConfig
	if err := json.Unmarshal(plain, &sc); err != nil {
		return nil, fmt.Errorf("autobak: config повреждён: %w", err)
	}
	r := &Repo{be: be, key: key, cfg: cfg, chp: sc.Chunker, idx: NewIndex()}
	if err := loadAllIndexes(ctx, be, key, r.idx); err != nil {
		return nil, err
	}
	return r, nil
}

func readConfig(ctx context.Context, be backend.Backend) (Config, error) {
	raw, err := backend.ReadAll(ctx, be, FileConfig, maxConfigSize)
	if errors.Is(err, backend.ErrNotFound) {
		return Config{}, ErrNotInitialized
	} else if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("autobak: config нечитаем: %w", err)
	}
	if cfg.Version != RepoVersion {
		return Config{}, fmt.Errorf("autobak: репозиторий версии %d, поддерживается %d", cfg.Version, RepoVersion)
	}
	return cfg, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("autobak: не сериализуется: " + err.Error())
	}
	return b
}

func putJSON(ctx context.Context, be backend.Backend, name string, v any) error {
	raw := mustJSON(v)
	return be.Put(ctx, name, bytes.NewReader(raw), int64(len(raw)))
}

// putJSONNew пишет объект, только если его ещё нет. Для создаваемых один раз
// объектов репозитория (config, ключи при инициализации).
func putJSONNew(ctx context.Context, be backend.Backend, name string, v any) error {
	raw := mustJSON(v)
	return be.PutNew(ctx, name, bytes.NewReader(raw), int64(len(raw)))
}

// --- Запись ---------------------------------------------------------------

type WriteStats struct {
	ChunksTotal int64
	ChunksNew   int64
	BytesTotal  int64 // сколько исходных данных прошло через writer
	BytesNew    int64 // сколько из них оказалось новыми
	BytesStored int64 // сколько реально ушло в хранилище (после сжатия)
}

// Writer накапливает чанки в паки и загружает их в фоне.
//
// Один Writer соответствует одному бэкапу. Он не потокобезопасен для
// параллельных WriteStream - сборщики работают последовательно, зато
// загрузка в хранилище идёт параллельно чтению следующих файлов, а именно
// она и есть узкое место.
type Writer struct {
	r   *Repo
	ctx context.Context

	pack *packBuilder
	// seen - все чанки, записанные этим writer'ом, за всё время его жизни.
	// Очищать его при отправке пака нельзя: между отправкой и попаданием
	// пака в индекс есть окно, и чанк из этого окна был бы записан повторно.
	seen map[ChunkID]struct{}

	sem chan struct{}
	wg  sync.WaitGroup

	mu       sync.Mutex
	err      error
	newPacks []indexPack
	saved    int
	stats    WriteStats
}

const uploadParallelism = 4

func (r *Repo) NewWriter(ctx context.Context) (*Writer, error) {
	if !r.be.Caps().CanWrite {
		return nil, backend.ErrReadOnly
	}
	pb, err := newPackBuilder()
	if err != nil {
		return nil, err
	}
	return &Writer{
		r: r, ctx: ctx, pack: pb,
		seen: map[ChunkID]struct{}{},
		sem:  make(chan struct{}, uploadParallelism),
	}, nil
}

func (w *Writer) fail(err error) {
	w.mu.Lock()
	if w.err == nil && err != nil {
		w.err = err
	}
	w.mu.Unlock()
}

func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// WriteChunk кладёт чанк в репозиторий, если такого там ещё нет.
// Второе возвращаемое значение - был ли чанк новым.
func (w *Writer) WriteChunk(plain []byte) (ChunkID, bool, error) {
	if err := w.Err(); err != nil {
		return ChunkID{}, false, err
	}
	id := w.r.key.ChunkID(plain)
	w.stats.ChunksTotal++
	w.stats.BytesTotal += int64(len(plain))

	if w.r.idx.Has(id) {
		return id, false, nil
	}
	if _, ok := w.seen[id]; ok {
		return id, false, nil // уже лежит в паке этого же бэкапа
	}

	n, err := w.pack.add(w.r.key, id, plain)
	if err != nil {
		return id, false, err
	}
	w.seen[id] = struct{}{}
	w.stats.ChunksNew++
	w.stats.BytesNew += int64(len(plain))
	w.stats.BytesStored += int64(n)

	if w.pack.size() >= packTargetSize {
		if err := w.flushPack(); err != nil {
			return id, true, err
		}
	}
	return id, true, nil
}

func (w *Writer) flushPack() error {
	if w.pack.count() == 0 {
		return nil
	}
	pb := w.pack
	next, err := newPackBuilder()
	if err != nil {
		return err
	}
	w.pack = next

	data, err := pb.finish(w.r.key)
	if err != nil {
		return err
	}
	entries := pb.entries

	select {
	case w.sem <- struct{}{}:
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
	w.wg.Add(1)
	go func() {
		defer func() { <-w.sem; w.wg.Done() }()
		err := w.r.be.PutNew(w.ctx, packName(pb.id), bytes.NewReader(data), int64(len(data)))
		if err != nil && !errors.Is(err, backend.ErrExists) {
			// ErrExists для пака - не ошибка: имя привязано к содержимому,
			// значит те же байты уже лежат (повтор после потерянного
			// ответа). Перезаписать чужой пак PutNew не даст.
			w.fail(fmt.Errorf("autobak: не записать пак %s: %w", pb.id, err))
			return
		}
		// Индекс обновляется только после успешной загрузки: иначе
		// следующий чанк сочли бы дубликатом того, чего в хранилище нет.
		w.r.idx.Add(pb.id, entries)
		w.mu.Lock()
		w.newPacks = append(w.newPacks, indexPack{ID: pb.id, Blobs: entries})
		w.mu.Unlock()
	}()

	return w.checkpoint(false)
}

// checkpoint периодически сбрасывает индекс на уже загруженные паки.
//
// Бэкап на терабайт идёт часами. Если он оборвётся, без промежуточных
// индексов вся загруженная работа окажется недостижимой (восстановимой
// через repair, но только вручную). Раз в 64 пака - это раз в гигабайт.
func (w *Writer) checkpoint(force bool) error {
	w.mu.Lock()
	n := len(w.newPacks)
	need := force || n-w.saved >= 64
	var batch []indexPack
	if need && n > w.saved {
		batch = append([]indexPack(nil), w.newPacks[w.saved:n]...)
		w.saved = n
	}
	w.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	_, err := saveIndex(w.ctx, w.r.be, w.r.key, batch)
	return err
}

// Close дописывает последний пак, дожидается всех загрузок и сохраняет
// индекс. Пока Close не вернул nil, снимок сохранять нельзя: он сослался бы
// на чанки, которых в хранилище может не оказаться.
func (w *Writer) Close() (WriteStats, error) {
	if err := w.flushPack(); err != nil {
		w.fail(err)
	}
	w.wg.Wait()
	if err := w.Err(); err != nil {
		return w.stats, err
	}
	if err := w.checkpoint(true); err != nil {
		return w.stats, err
	}
	return w.stats, nil
}

// --- Чтение ---------------------------------------------------------------

// LoadChunk достаёт чанк из репозитория, проверяя подпись и распаковывая.
func (r *Repo) LoadChunk(ctx context.Context, id ChunkID) ([]byte, error) {
	packID, loc, ok := r.idx.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("autobak: чанк %s не найден в индексе", id.String()[:12])
	}
	sealed, err := r.readBlob(ctx, packID, loc)
	if err != nil {
		return nil, err
	}
	payload, err := r.key.OpenChunk(id, sealed)
	if err != nil {
		return nil, fmt.Errorf("autobak: чанк %s: %w", id.String()[:12], err)
	}
	if !loc.Comp {
		if len(payload) != int(loc.PLen) {
			return nil, fmt.Errorf("autobak: чанк %s имеет неожиданный размер", id.String()[:12])
		}
		return payload, nil
	}
	return decompress(payload, int(loc.PLen))
}

// readBlob берёт кусок пака, по возможности из кэша целого пака.
func (r *Repo) readBlob(ctx context.Context, packID string, loc Location) ([]byte, error) {
	r.cacheMu.Lock()
	if r.cacheID == packID && uint64(len(r.cacheB)) >= loc.Off+uint64(loc.Len) {
		out := append([]byte(nil), r.cacheB[loc.Off:loc.Off+uint64(loc.Len)]...)
		r.cacheMu.Unlock()
		return out, nil
	}
	r.cacheMu.Unlock()

	name := packName(packID)
	st, err := r.be.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	// Пак умеренного размера выгоднее скачать целиком: следующие чанки того
	// же файла почти наверняка лежат рядом, и это сэкономит десятки запросов.
	if st.Size <= packTargetSize+2*MiB {
		data, err := backend.ReadAll(ctx, r.be, name, packTargetSize+4*MiB)
		if err != nil {
			return nil, err
		}
		if uint64(len(data)) < loc.Off+uint64(loc.Len) {
			return nil, fmt.Errorf("autobak: пак %s короче, чем указано в индексе", packID)
		}
		r.cacheMu.Lock()
		r.cacheID, r.cacheB = packID, data
		r.cacheMu.Unlock()
		return data[loc.Off : loc.Off+uint64(loc.Len)], nil
	}
	return readRange(ctx, r.be, name, int64(loc.Off), int64(loc.Len))
}

// ReadStream склеивает чанки обратно в поток.
func (r *Repo) ReadStream(ctx context.Context, chunks []ChunkID, w io.Writer) (int64, error) {
	var total int64
	for _, id := range chunks {
		data, err := r.LoadChunk(ctx, id)
		if err != nil {
			return total, err
		}
		n, err := w.Write(data)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// --- Снимки ---------------------------------------------------------------

func (r *Repo) SaveSnapshot(ctx context.Context, s *Snapshot) error {
	if s.ID == "" {
		id, err := randHex(8)
		if err != nil {
			return err
		}
		s.ID = id
	}
	name := DirSnapshots + "/" + s.ID
	sealed, err := r.key.SealMeta(name, mustJSON(s))
	if err != nil {
		return err
	}
	// Снимок неизменяем, имя уникально: PutNew исключает перезапись
	// существующего манифеста. ErrExists здесь означало бы повтор с тем
	// же ID после сбоя сети - те же байты, поэтому не ошибка.
	if err := r.be.PutNew(ctx, name, bytes.NewReader(sealed), int64(len(sealed))); err != nil &&
		!errors.Is(err, backend.ErrExists) {
		return err
	}
	return nil
}

func (r *Repo) LoadSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	name := DirSnapshots + "/" + id
	raw, err := backend.ReadAll(ctx, r.be, name, maxSnapshotSize)
	if err != nil {
		return nil, err
	}
	plain, err := r.key.OpenMeta(name, raw)
	if err != nil {
		return nil, fmt.Errorf("autobak: снимок %s: %w", id, err)
	}
	var s Snapshot
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, fmt.Errorf("autobak: снимок %s повреждён: %w", id, err)
	}
	return &s, nil
}

// ListSnapshots возвращает снимки от новых к старым.
// ListSnapshots возвращает читаемые снимки, молча пропуская битые.
//
// Годится для показа в интерфейсе: один нечитаемый снимок не должен
// прятать все остальные. Для prune это опасно (пропущенный снимок счёлся
// бы удалённым, и его чанки стёрли бы), поэтому очистка берёт строгий
// вариант - listSnapshotsStrict.
func (r *Repo) ListSnapshots(ctx context.Context) ([]*Snapshot, error) {
	snaps, _, err := r.loadSnapshots(ctx)
	return snaps, err
}

// listSnapshotsStrict возвращает ошибку, если хоть один манифест не
// прочитался: для prune и verify нечитаемый снимок - это стоп, а не пропуск.
func (r *Repo) listSnapshotsStrict(ctx context.Context) ([]*Snapshot, error) {
	snaps, failed, err := r.loadSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("autobak: не прочитать снимки (%d), операция остановлена во избежание потери данных: %s",
			len(failed), strings.Join(failed, ", "))
	}
	return snaps, nil
}

// loadSnapshots читает все манифесты и отдельно возвращает id тех, что не
// прочитались, чтобы вызывающий сам решил, критично это или нет.
func (r *Repo) loadSnapshots(ctx context.Context) (snaps []*Snapshot, failed []string, err error) {
	var ids []string
	if err := r.be.List(ctx, DirSnapshots+"/", func(fi backend.FileInfo) error {
		if id := strings.TrimPrefix(fi.Name, DirSnapshots+"/"); id != "" && !strings.Contains(id, "/") {
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}

	out := make([]*Snapshot, len(ids))
	errs := make([]error, len(ids))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i], errs[i] = r.LoadSnapshot(ctx, id)
		}()
	}
	wg.Wait()

	res := make([]*Snapshot, 0, len(ids))
	for i, s := range out {
		if errs[i] != nil {
			failed = append(failed, ids[i])
			continue
		}
		res = append(res, s)
	}
	sortSnapshotsDesc(res)
	return res, failed, nil
}

func sortSnapshotsDesc(s []*Snapshot) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Time.After(s[j-1].Time); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (r *Repo) DeleteSnapshot(ctx context.Context, id string) error {
	return r.be.Delete(ctx, DirSnapshots+"/"+id)
}

// ReadTree отдаёт узлы снимка по одному.
//
// Поток разбирается на лету: дерево на миллион файлов весит сотни мегабайт,
// и держать его в памяти целиком ради показа списка нет причин.
func (r *Repo) ReadTree(ctx context.Context, s *Snapshot, fn func(*Node) error) error {
	pr, pw := io.Pipe()
	go func() {
		_, err := r.ReadStream(ctx, s.Tree, pw)
		pw.CloseWithError(err)
	}()
	defer pr.Close()

	dec := json.NewDecoder(pr)
	for {
		var n Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("autobak: дерево снимка %s повреждено: %w", s.ID, err)
		}
		if err := fn(&n); err != nil {
			return err
		}
	}
}
