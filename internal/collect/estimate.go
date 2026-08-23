package collect

import (
	"context"
	"io/fs"
	"path/filepath"

	"github.com/iamtime/autobak/internal/plan"
)

// Estimate - предварительная оценка объёма бэкапа, снятая быстрым обходом
// без чтения содержимого файлов.
type Estimate struct {
	// Bytes - сколько данных будет прочитано (с учётом исключений плана).
	// Не то же, что размер в хранилище: там будет меньше за счёт сжатия и
	// дедупликации, но это честная верхняя граница «сколько читать».
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
	// Partial - часть модулей не посчитана точно (например, базы: их размер
	// дампа заранее не известен). Интерфейс обязан это показать.
	Partial bool     `json:"partial,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
}

// EstimatePlan быстро оценивает объём файловых модулей и docker-томов.
//
// Обход только по метаданным (stat), содержимое не читается, поэтому даже
// сотни тысяч файлов считаются за секунды. Базы и k8s в точную оценку не
// входят: размер дампа заранее не предсказать - они помечаются в Skipped.
func EstimatePlan(ctx context.Context, p *plan.Plan) Estimate {
	var e Estimate
	for _, m := range p.Enabled() {
		if err := ctx.Err(); err != nil {
			return e
		}
		switch m.Kind {
		case plan.KindFiles, plan.KindConfigs, plan.KindHestia:
			match := NewMatcher(append(append([]string{}, p.Excludes...), m.Excludes...)...)
			for _, root := range m.Paths {
				walkSize(ctx, root, match, &e)
			}
		case plan.KindDocker:
			vols := m.Volumes
			if len(vols) == 0 {
				vols = dockerVolumeNames(ctx)
			}
			for _, v := range vols {
				mp := volumeMount(ctx, v)
				if mp != "" {
					walkSize(ctx, mp, nil, &e)
				}
			}
		case plan.KindMySQL, plan.KindPostgres, plan.KindK8s:
			// Дамп базы и манифесты кластера заранее не измерить точно.
			e.Partial = true
			e.Skipped = append(e.Skipped, m.Name)
		}
	}
	return e
}

// walkSize суммирует размеры файлов под root, пропуская то, что отсекает
// матчер исключений (nil - ничего не исключать). Каталог-исключение
// не обходится вовсе.
func walkSize(ctx context.Context, root string, m *Matcher, e *Estimate) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // нечитаемое пропускаем, как и сам бэкап
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if m != nil && m.Match(filepath.ToSlash(p)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, e2 := d.Info(); e2 == nil {
				e.Bytes += info.Size()
				e.Files++
			}
		}
		return nil
	})
}

// volumeMount и dockerVolumeNames - тонкие обёртки над docker для оценки.
func volumeMount(ctx context.Context, name string) string {
	c := &dockerCollector{}
	return c.volumeMountpoint(ctx, name)
}

func dockerVolumeNames(ctx context.Context) []string {
	out, err := runCapture(ctx, "docker", "volume", "ls", "-q")
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range trimLines(out) {
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}
