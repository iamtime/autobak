// Package engine связывает сборщики с репозиторием.
//
// Три сценария, один код:
//
//	Backup - сборщики пишут прямо в репозиторий (push-режим, агент на сервере)
//	Export - сборщики пишут в поток кадров  (агент отдаёт данные по SSH)
//	Import - поток кадров пишется в репозиторий (десктоп принимает по SSH)
//
// Export и Import - это тот же Backup, разрезанный пополам по SSH-каналу.
// Благодаря этому оба режима дают побайтово одинаковые снимки, и переезд
// с локального репозитория на S3 не обесценивает накопленную историю.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
)

type Options struct {
	Plan   *plan.Plan
	Server string
	Tags   []string
	Agent  string
	// Parent - предыдущий снимок этого сервера. Только для истории:
	// дедупликация работает по всему репозиторию и в родителе не нуждается.
	Parent string

	// Known - чанки, которые этот сервер присылал раньше. По ним
	// отвечаем агенту, что передавать повторно не нужно.
	Known map[repo.ChunkID]struct{}

	Progress func(proto.Progress)
	Log      func(level, msg string)
}

func (o *Options) progress(p proto.Progress) {
	if o.Progress != nil {
		o.Progress(p)
	}
}

func (o *Options) log(level, msg string) {
	if o.Log != nil {
		o.Log(level, msg)
	}
}

// --- Приёмник, пишущий в репозиторий --------------------------------------

type repoSink struct {
	w    *repo.Writer
	tree *repo.StreamWriter
	enc  *json.Encoder
	opt  *Options

	module string
	files  int64
	dirs   int64
	bytes  int64

	// modBase запоминает счётчики на начало модуля, чтобы отчитаться
	// по каждому отдельно, а не только суммарно.
	modFiles, modBytes int64

	lastReport time.Time
}

func newRepoSink(w *repo.Writer, opt *Options) *repoSink {
	tree := w.NewStream()
	return &repoSink{w: w, tree: tree, enc: json.NewEncoder(tree), opt: opt}
}

func (s *repoSink) Meta(n *repo.Node) error {
	if n.Type == repo.NodeDir {
		s.dirs++
	}
	return s.enc.Encode(n)
}

func (s *repoSink) File(n *repo.Node, r io.Reader) error {
	ids, size, err := s.w.WriteStream(r)
	if err != nil {
		return err
	}
	// Размер берём фактический, а не из lstat: между обходом каталога и
	// чтением файл мог измениться, и записать в дерево устаревшую длину
	// значило бы получить снимок, который не сойдётся при восстановлении.
	n.Chunks, n.Size = ids, size
	s.files++
	s.bytes += size
	s.report(n.Path)
	return s.enc.Encode(n)
}

func (s *repoSink) Logf(level, format string, args ...any) {
	s.opt.log(level, fmt.Sprintf(format, args...))
}

func (s *repoSink) Progress(path string, bytes int64) {
	s.bytes += bytes
	s.report(path)
}

// report ограничивает частоту событий прогресса.
//
// Без этого на сайте из 200 тысяч мелких файлов интерфейс получал бы
// 200 тысяч сообщений и тратил бы на перерисовку больше времени, чем
// бэкап - на работу.
func (s *repoSink) report(path string) {
	now := time.Now()
	if now.Sub(s.lastReport) < 100*time.Millisecond {
		return
	}
	s.lastReport = now
	s.opt.progress(proto.Progress{
		Stage: s.module, Path: path, Files: s.files, Bytes: s.bytes,
	})
}

// --- Бэкап напрямую в репозиторий -----------------------------------------

func Backup(ctx context.Context, r *repo.Repo, opt Options) (*repo.Snapshot, error) {
	if err := opt.Plan.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	// Общая блокировка на всё время бэкапа. Без неё очистка, запущенная
	// параллельно, удалит наши паки как ничейные: снимок ещё не сохранён,
	// и ссылаться на них некому.
	lock, err := r.LockWithRetry(ctx, "бэкап "+opt.Server, false, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	w, err := r.NewWriter(ctx)
	if err != nil {
		return nil, err
	}
	sink := newRepoSink(w, &opt)

	modules, fatal := runCollectors(ctx, opt.Plan, sink, &opt)
	if fatal != nil {
		return nil, fatal
	}

	tree, _, err := sink.tree.Close()
	if err != nil {
		return nil, err
	}
	stats, err := w.Close()
	if err != nil {
		return nil, err
	}

	snap := &repo.Snapshot{
		Time: start, Server: opt.Server, Hostname: hostname(),
		Parent: opt.Parent, Tags: opt.Tags, Agent: opt.Agent,
		Modules: modules, Tree: tree,
		Stats: repo.SnapshotStats{
			Files: sink.files, Dirs: sink.dirs,
			BytesTotal: stats.BytesTotal, BytesNew: stats.BytesNew,
			BytesStored: stats.BytesStored,
			ChunksTotal: stats.ChunksTotal, ChunksNew: stats.ChunksNew,
			DurationMS: time.Since(start).Milliseconds(),
		},
	}
	// Если за время бэкапа блокировка успела протухнуть (связь с
	// хранилищем пропадала дольше lockStale), сохранять снимок нельзя:
	// пока наша отметка живости молчала, очистка на другой машине могла
	// счесть репозиторий свободным и удалить уже залитые паки как ничейные.
	// Снимок сослался бы на пустоту. Лучше честно провалить бэкап.
	if lock.Lost() {
		return nil, errors.New("autobak: связь с хранилищем прерывалась дольше допустимого, " +
			"блокировка могла протухнуть - снимок не сохранён во избежание ссылок на удалённые данные")
	}

	// Снимок сохраняется последним: до этого момента он ссылался бы на
	// чанки, часть которых могла не долететь до хранилища.
	if err := r.SaveSnapshot(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// runCollectors прогоняет модули плана.
//
// Ошибка модуля не прекращает бэкап: три базы из четырёх и все файлы
// лучше, чем ничего. Снимок при этом помечается как неполный - интерфейс
// обязан показать это, иначе неполный бэкап будет выглядеть как обычный.
func runCollectors(ctx context.Context, p *plan.Plan, sink *repoSink, opt *Options) ([]repo.Module, error) {
	var out []repo.Module
	for _, pm := range p.Enabled() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		sink.module = pm.Kind.Title() + ": " + pm.Name
		sink.modFiles, sink.modBytes = sink.files, sink.bytes
		opt.progress(proto.Progress{Stage: sink.module, Files: sink.files, Bytes: sink.bytes})

		c, err := collect.New(pm, p.Excludes)
		if err != nil {
			out = append(out, repo.Module{Kind: string(pm.Kind), Name: pm.Name, Err: err.Error()})
			continue
		}
		if fc, ok := c.(interface{ SetMaxFileSize(int64) }); ok && p.MaxFileSize > 0 {
			fc.SetMaxFileSize(p.MaxFileSize)
		}

		meta, cerr := c.Collect(ctx, sink)
		m := repo.Module{
			Kind: string(pm.Kind), Name: pm.Name, Meta: meta,
			Files: sink.files - sink.modFiles,
			Bytes: sink.bytes - sink.modBytes,
		}
		if cerr != nil {
			// Отмена - это не отказ модуля, а решение человека:
			// продолжать остальные модули бессмысленно.
			if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
				return out, cerr
			}
			m.Err = cerr.Error()
			opt.log("error", pm.Name+": "+cerr.Error())
		}
		out = append(out, m)
	}
	return out, nil
}
