package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/iamtime/autobak/internal/backend"
)

// Index - карта «чанк → где он лежит», загружаемая в память целиком.
//
// Именно она делает дедупликацию возможной: перед записью чанка достаточно
// заглянуть в карту, а не опрашивать хранилище. Расход памяти - примерно
// 90 байт на чанк, то есть около 18 МБ на 100 ГБ данных при среднем чанке
// в 512 КБ. Для терабайтных репозиториев это станет заметно, и тогда
// понадобится вынести карту на диск; пока такой цены платить незачем.
type Index struct {
	mu      sync.RWMutex
	packs   []string
	packRef map[string]uint32
	m       map[ChunkID]Location
}

type Location struct {
	pack uint32
	Off  uint64
	Len  uint32
	PLen uint32
	Comp bool
}

type indexPack struct {
	ID    string      `json:"id"`
	Blobs []blobEntry `json:"b"`
}

type indexFile struct {
	Version int         `json:"version"`
	Packs   []indexPack `json:"packs"`
}

func NewIndex() *Index {
	return &Index{packRef: map[string]uint32{}, m: map[ChunkID]Location{}}
}

func (i *Index) Add(packID string, blobs []blobEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	ref, ok := i.packRef[packID]
	if !ok {
		ref = uint32(len(i.packs))
		i.packs = append(i.packs, packID)
		i.packRef[packID] = ref
	}
	for _, b := range blobs {
		// Первое вхождение выигрывает. Дубль означает, что один и тот же
		// чанк попал в два пака (гонка двух параллельных бэкапов); данные
		// в обоих одинаковые, так что достаточно любой ссылки.
		if _, exists := i.m[b.ID]; exists {
			continue
		}
		i.m[b.ID] = Location{pack: ref, Off: b.Off, Len: b.Len, PLen: b.PLen, Comp: b.Comp}
	}
}

func (i *Index) Lookup(id ChunkID) (packID string, loc Location, ok bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	loc, ok = i.m[id]
	if !ok {
		return "", Location{}, false
	}
	return i.packs[loc.pack], loc, true
}

func (i *Index) Has(id ChunkID) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.m[id]
	return ok
}

func (i *Index) Count() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.m)
}

func (i *Index) PackIDs() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]string(nil), i.packs...)
}

// Each обходит все чанки. Нужен prune, чтобы понять, какие паки ещё
// удерживаются живыми снимками, а какие можно удалить целиком.
func (i *Index) Each(fn func(id ChunkID, packID string, loc Location) bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	for id, loc := range i.m {
		if !fn(id, i.packs[loc.pack], loc) {
			return
		}
	}
}

func saveIndex(ctx context.Context, be backend.Backend, key *MasterKey, packs []indexPack) (string, error) {
	if len(packs) == 0 {
		return "", nil
	}
	id, err := newPackID()
	if err != nil {
		return "", err
	}
	name := DirIndex + "/" + id
	raw, err := json.Marshal(indexFile{Version: 1, Packs: packs})
	if err != nil {
		return "", err
	}
	sealed, err := key.SealMeta(name, raw)
	if err != nil {
		return "", err
	}
	// Индекс неизменяем, id уникален: PutNew исключает перезапись. ErrExists
	// означало бы повтор с тем же id после сбоя сети - те же байты.
	if err := be.PutNew(ctx, name, bytes.NewReader(sealed), int64(len(sealed))); err != nil &&
		!errors.Is(err, backend.ErrExists) {
		return "", fmt.Errorf("autobak: не записать индекс: %w", err)
	}
	return id, nil
}

func loadIndexFile(ctx context.Context, be backend.Backend, key *MasterKey, name string) (*indexFile, error) {
	raw, err := backend.ReadAll(ctx, be, name, maxIndexSize)
	if err != nil {
		return nil, err
	}
	plain, err := key.OpenMeta(name, raw)
	if err != nil {
		return nil, fmt.Errorf("autobak: индекс %s: %w", name, err)
	}
	var f indexFile
	if err := json.Unmarshal(plain, &f); err != nil {
		return nil, fmt.Errorf("autobak: индекс %s повреждён: %w", name, err)
	}
	return &f, nil
}

// loadAllIndexes читает каталог index/ в память.
//
// Файлы тянутся параллельно: в S3 каждый запрос стоит ~50 мс задержки,
// и последовательное чтение сотни индексов превратилось бы в пять секунд
// ожидания перед стартом любой операции.
func loadAllIndexes(ctx context.Context, be backend.Backend, key *MasterKey, idx *Index) error {
	var names []string
	err := be.List(ctx, DirIndex+"/", func(fi backend.FileInfo) error {
		if strings.HasPrefix(fi.Name, DirIndex+"/") {
			names = append(names, fi.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}

	const workers = 8
	ch := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for range min(workers, max(len(names), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range ch {
				f, err := loadIndexFile(ctx, be, key, name)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					for _, p := range f.Packs {
						idx.Add(p.ID, p.Blobs)
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, n := range names {
		select {
		case ch <- n:
		case <-ctx.Done():
		}
	}
	close(ch)
	wg.Wait()
	return firstErr
}
