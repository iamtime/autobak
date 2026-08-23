package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

// Зеркалирование репозитория - правило «три копии, два носителя, одна
// вне площадки».
//
// Копирование идёт на уровне объектов хранилища, без расшифровки: ключ
// для зеркала не нужен вовсе, и зеркало открывается тем же паролем, что
// и оригинал. Побочное следствие приятное - зеркало можно делать на
// машине, которой вы не доверяете настолько, чтобы дать ей ключи.

type MirrorOptions struct {
	DryRun bool
	// Prune удаляет из зеркала то, чего больше нет в источнике.
	//
	// Выключено по умолчанию и это не осторожность ради осторожности:
	// зеркало существует ровно на случай, когда с источником случилось
	// плохое. Ошибка в источнике (или злой умысел) не должна
	// распространяться на копию автоматически.
	Prune bool
	// Verify после копирования читает зеркало целиком и проверяет подписи.
	Verify bool

	Workers  int
	Progress func(stage string, done, total int)
}

func DefaultMirrorOptions() MirrorOptions {
	return MirrorOptions{Workers: 8}
}

type MirrorReport struct {
	Total    int      `json:"total"`
	Copied   int      `json:"copied"`
	Skipped  int      `json:"skipped"`
	Deleted  int      `json:"deleted"`
	Orphans  []string `json:"orphans,omitempty"`
	Bytes    int64    `json:"bytes"`
	Problems []string `json:"problems,omitempty"`
	DryRun   bool     `json:"dry_run"`
	Duration Duration `json:"duration"`
}

func (r *MirrorReport) OK() bool { return len(r.Problems) == 0 }

func (r *MirrorReport) Summary() string {
	verb := "скопировано"
	if r.DryRun {
		verb = "будет скопировано"
	}
	s := fmt.Sprintf("%s объектов: %d из %d (%s), уже было: %d",
		verb, r.Copied, r.Total, HumanBytes(r.Bytes), r.Skipped)
	if r.Deleted > 0 {
		s += fmt.Sprintf(", удалено лишних: %d", r.Deleted)
	} else if n := len(r.Orphans); n > 0 {
		s += fmt.Sprintf(", лишних в зеркале: %d", n)
	}
	if len(r.Problems) > 0 {
		s += fmt.Sprintf("; ПРОБЛЕМ: %d", len(r.Problems))
	}
	return s
}

// Mirror копирует репозиторий в другое хранилище.
//
// Порядок копирования выбран так, чтобы прерванное зеркало всегда
// оставалось пригодным к восстановлению: сначала ключи и настройки,
// затем данные, затем индексы и только в самом конце снимки. Оборвись
// копирование в любой момент - в зеркале окажется меньше снимков, но
// каждый из оставшихся будет полным. Обратный порядок дал бы снимки,
// ссылающиеся на несуществующие данные, то есть ложное чувство
// сохранности.
func Mirror(ctx context.Context, src, dst backend.Backend, opt MirrorOptions) (*MirrorReport, error) {
	start := time.Now()
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	if !opt.DryRun && !dst.Caps().CanWrite {
		return nil, backend.ErrReadOnly
	}
	progress := opt.Progress
	if progress == nil {
		progress = func(string, int, int) {}
	}
	rep := &MirrorReport{DryRun: opt.DryRun}

	stages := []struct {
		name   string
		prefix string
	}{
		{"ключи и настройки", ""},
		{"данные", DirData + "/"},
		{"индексы", DirIndex + "/"},
		{"снимки", DirSnapshots + "/"},
	}

	// Содержимое зеркала читается один раз: спрашивать про каждый объект
	// отдельно означало бы удвоить число запросов к хранилищу.
	have, err := listSizes(ctx, dst, "")
	if err != nil {
		return nil, fmt.Errorf("autobak: не прочитать содержимое зеркала: %w", err)
	}

	seen := map[string]bool{}
	for _, st := range stages {
		names, err := listStage(ctx, src, st.prefix)
		if err != nil {
			return nil, fmt.Errorf("autobak: не прочитать источник: %w", err)
		}
		rep.Total += len(names)
		for _, fi := range names {
			seen[fi.Name] = true
		}
		if err := copyStage(ctx, src, dst, st.name, names, have, opt, rep, progress); err != nil {
			return rep, err
		}
	}

	// Лишнее в зеркале - след давнего prune в источнике.
	for name := range have {
		if !seen[name] {
			rep.Orphans = append(rep.Orphans, name)
		}
	}
	if opt.Prune && !opt.DryRun && dst.Caps().CanDelete {
		for i, name := range rep.Orphans {
			progress("удаление лишнего", i, len(rep.Orphans))
			if err := dst.Delete(ctx, name); err != nil {
				rep.Problems = append(rep.Problems, name+": "+err.Error())
				continue
			}
			rep.Deleted++
		}
	}

	rep.Duration = Duration(time.Since(start))
	return rep, nil
}

func listStage(ctx context.Context, be backend.Backend, prefix string) ([]backend.FileInfo, error) {
	var out []backend.FileInfo
	err := be.List(ctx, prefix, func(fi backend.FileInfo) error {
		// Пустой префикс означает «всё, кроме того, что копируется
		// отдельными этапами»: config и keys.
		if prefix == "" && strings.ContainsRune(fi.Name, '/') &&
			!strings.HasPrefix(fi.Name, DirKeys+"/") {
			return nil
		}
		out = append(out, fi)
		return nil
	})
	return out, err
}

func listSizes(ctx context.Context, be backend.Backend, prefix string) (map[string]int64, error) {
	out := map[string]int64{}
	err := be.List(ctx, prefix, func(fi backend.FileInfo) error {
		out[fi.Name] = fi.Size
		return nil
	})
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return nil, err
	}
	return out, nil
}

func copyStage(ctx context.Context, src, dst backend.Backend, stage string,
	names []backend.FileInfo, have map[string]int64,
	opt MirrorOptions, rep *MirrorReport, progress func(string, int, int)) error {

	var mu sync.Mutex
	var done int
	sem := make(chan struct{}, opt.Workers)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, fi := range names {
		if err := ctx.Err(); err != nil {
			break
		}
		// Объекты репозитория неизменяемы: одинаковое имя и размер
		// означают одинаковое содержимое. Сравнивать побайтово незачем,
		// а вот перезаписывать уже скопированное - значит превращать
		// докачку в полную копию каждый раз.
		if size, ok := have[fi.Name]; ok && size == fi.Size {
			mu.Lock()
			rep.Skipped++
			mu.Unlock()
			continue
		}
		if opt.DryRun {
			mu.Lock()
			rep.Copied++
			rep.Bytes += fi.Size
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			err := copyObject(ctx, src, dst, fi)
			mu.Lock()
			defer mu.Unlock()
			done++
			progress(stage, done, len(names))
			if err != nil {
				rep.Problems = append(rep.Problems, fi.Name+": "+err.Error())
				return
			}
			rep.Copied++
			rep.Bytes += fi.Size
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func copyObject(ctx context.Context, src, dst backend.Backend, fi backend.FileInfo) error {
	rc, err := src.Get(ctx, fi.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	// Объекты репозитория неизменяемы: если он уже есть в зеркале - это те
	// же байты, копировать нечего. PutNew заодно не даёт перезаписать в
	// зеркале то, что там уже лежит.
	if err := dst.PutNew(ctx, fi.Name, rc, fi.Size); err != nil && !errors.Is(err, backend.ErrExists) {
		return err
	}
	return nil
}

// MirrorAndVerify копирует репозиторий и проверяет получившееся зеркало.
//
// Проверка отдельным шагом и по требованию: она читает зеркало целиком,
// то есть для S3 это оплаченный трафик за все данные. Но копия, которую
// ни разу не прочитали, копией считаться не может.
func MirrorAndVerify(ctx context.Context, src, dst backend.Backend, password string,
	opt MirrorOptions) (*MirrorReport, *VerifyReport, error) {

	rep, err := Mirror(ctx, src, dst, opt)
	if err != nil || opt.DryRun || !opt.Verify {
		return rep, nil, err
	}
	mirror, err := Open(ctx, dst, password)
	if err != nil {
		return rep, nil, fmt.Errorf("autobak: зеркало не открывается: %w", err)
	}
	vr, err := mirror.Verify(ctx, VerifyOptions{Sample: 1})
	return rep, vr, err
}
