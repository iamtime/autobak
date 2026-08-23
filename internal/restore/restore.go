// Package restore возвращает данные из снимка.
//
// Устроен зеркально бэкапу: драйвер читает дерево снимка и отдаёт узлы в
// Target. Target - либо файловая система (агент раскладывает данные на
// сервере, десктоп скачивает к себе), либо поток кадров к агенту по SSH.
//
// Главный принцип: по умолчанию ничего не перезаписывается. Восстановление
// поверх работающего сайта - отдельное осознанное действие, а не значение
// по умолчанию, потому что ошибка здесь необратима и стоит дороже, чем
// сама авария, из-за которой восстановление затеяли.
package restore

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/repo"
)

// Target принимает восстанавливаемые узлы.
//
// content не равен nil только для обычных файлов и должен быть прочитан
// до конца до возврата из Node.
type Target interface {
	Node(n *repo.Node, content io.Reader) error
	Finish() error
}

type Options struct {
	// Include ограничивает восстановление подмножеством путей.
	// Пусто - весь снимок. Пути сравниваются как префиксы:
	// "/home/admin/web/site.ru" вытащит весь сайт.
	Include []string
	// Modules ограничивает восстановление модулями снимка.
	Modules []string

	// DryRun только считает и ничего не пишет. Интерфейс обязан показать
	// результат до того, как спросить подтверждение.
	DryRun bool

	Progress func(done, total int64, path string)
	Log      func(level, msg string)
}

type Report struct {
	Files      int64    `json:"files"`
	Dirs       int64    `json:"dirs"`
	Symlinks   int64    `json:"symlinks"`
	Bytes      int64    `json:"bytes"`
	Databases  []string `json:"databases,omitempty"`
	Overwrites []string `json:"overwrites,omitempty"`
	Skipped    []string `json:"skipped,omitempty"`
	Problems   []string `json:"problems,omitempty"`
	DryRun     bool     `json:"dry_run"`
	// OverwriteUnknown означает, что список перезаписи в этом отчёте
	// неполон и на него нельзя опираться. Так помечается предпросмотр
	// восстановления НА СЕРВЕР: сухой прогон идёт локально и не видит, что
	// уже лежит на сервере, поэтому «перезаписей нет» было бы враньём.
	OverwriteUnknown bool   `json:"overwrite_unknown,omitempty"`
	Duration         string `json:"duration"`
}

func (r *Report) Summary() string {
	s := fmt.Sprintf("файлов %d (%s), каталогов %d",
		r.Files, repo.HumanBytes(r.Bytes), r.Dirs)
	if len(r.Databases) > 0 {
		s += fmt.Sprintf(", баз %d", len(r.Databases))
	}
	if n := len(r.Overwrites); n > 0 {
		s += fmt.Sprintf("; БУДЕТ ПЕРЕЗАПИСАНО: %d", n)
	}
	if len(r.Problems) > 0 {
		s += fmt.Sprintf("; проблем: %d", len(r.Problems))
	}
	return s
}

func (o *Options) log(level, msg string) {
	if o.Log != nil {
		o.Log(level, msg)
	}
}

// Run читает снимок и отдаёт отобранные узлы в Target.
func Run(ctx context.Context, r *repo.Repo, snap *repo.Snapshot, opt Options, t Target) (*Report, error) {
	start := time.Now()
	rep := &Report{DryRun: opt.DryRun}

	// Общий объём считается заранее - иначе полоса прогресса покажет
	// проценты от неизвестного и будет врать.
	var total int64
	if err := r.ReadTree(ctx, snap, func(n *repo.Node) error {
		if selects(n, opt) {
			total += n.Size
		}
		return nil
	}); err != nil {
		return nil, err
	}

	var done int64
	err := r.ReadTree(ctx, snap, func(n *repo.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !selects(n, opt) {
			return nil
		}
		if n.Err != "" {
			// Узел, который не удалось прочитать при бэкапе. Восстанавливать
			// нечего, но промолчать нельзя - иначе пропажа обнаружится позже.
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("%s: не был сохранён (%s)", n.Path, n.Err))
			return nil
		}

		switch n.Type {
		case repo.NodeDir:
			rep.Dirs++
		case repo.NodeSymlink:
			rep.Symlinks++
		case repo.NodeFile:
			rep.Files++
			rep.Bytes += n.Size
			if db := databaseOf(n.Path); db != "" {
				rep.Databases = append(rep.Databases, db)
			}
		}

		if opt.DryRun {
			if ow, ok := t.(interface{ WouldOverwrite(*repo.Node) bool }); ok && ow.WouldOverwrite(n) {
				rep.Overwrites = append(rep.Overwrites, n.Path)
			}
			return nil
		}

		var content io.Reader
		if n.Type == repo.NodeFile {
			content = &chunkReader{ctx: ctx, r: r, chunks: n.Chunks}
		}
		if err := t.Node(n, content); err != nil {
			return fmt.Errorf("%s: %w", n.Path, err)
		}
		done += n.Size
		if opt.Progress != nil {
			opt.Progress(done, total, n.Path)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	if !opt.DryRun {
		if err := t.Finish(); err != nil {
			return rep, err
		}
	}
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

func selects(n *repo.Node, opt Options) bool {
	if len(opt.Modules) > 0 {
		found := false
		for _, m := range opt.Modules {
			if n.Module == m {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(opt.Include) == 0 {
		return true
	}
	for _, inc := range opt.Include {
		inc = path.Clean(inc)
		if n.Path == inc || strings.HasPrefix(n.Path, inc+"/") {
			return true
		}
		// Родительские каталоги выбранного пути тоже нужны - иначе файл
		// окажется некуда положить.
		if strings.HasPrefix(inc, n.Path+"/") && n.Type == repo.NodeDir {
			return true
		}
	}
	return false
}

// databaseOf распознаёт виртуальные пути дампов баз.
func databaseOf(p string) string {
	for _, pref := range []string{collect.VirtualMySQL, collect.VirtualPostgres} {
		if rest, ok := strings.CutPrefix(p, pref+"/"); ok {
			if strings.HasPrefix(rest, "@") {
				return "" // @grants.sql, @globals.sql - не базы
			}
			return strings.TrimSuffix(strings.TrimSuffix(rest, ".sql"), ".dump")
		}
	}
	return ""
}

// chunkReader склеивает чанки файла в поток.
//
// Читает по одному чанку за раз, поэтому восстановление файла на 50 ГБ
// требует памяти под один чанк, а не под файл.
type chunkReader struct {
	ctx    context.Context
	r      *repo.Repo
	chunks []repo.ChunkID
	buf    []byte
	err    error
}

func (cr *chunkReader) Read(p []byte) (int, error) {
	for len(cr.buf) == 0 {
		if cr.err != nil {
			return 0, cr.err
		}
		if len(cr.chunks) == 0 {
			cr.err = io.EOF
			return 0, io.EOF
		}
		data, err := cr.r.LoadChunk(cr.ctx, cr.chunks[0])
		if err != nil {
			cr.err = err
			return 0, err
		}
		cr.chunks = cr.chunks[1:]
		cr.buf = data
	}
	n := copy(p, cr.buf)
	cr.buf = cr.buf[n:]
	return n, nil
}
