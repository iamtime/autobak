package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/iamtime/autobak/internal/repo"
)

// Учебное восстановление: снимок действительно раскладывается на диск,
// а получившиеся файлы сверяются с тем, что записано в снимке.
//
// Это принципиально сильнее проверки целостности. Verify читает чанки и
// проверяет подписи - то есть отвечает на вопрос «данные целы?». Здесь
// проверяется другое: «из них получается то же самое?». Между этими
// вопросами помещается весь путь - поиск по индексу, чтение пака,
// расшифровка, распаковка, склейка и запись на диск. Любое звено этого
// пути может сломаться так, что подписи останутся верными.
//
// Проверять весь снимок обычно нельзя: под сотню гигабайт нужны сотня
// гигабайт свободного места и часы времени. Поэтому берётся случайная
// выборка в пределах заданного объёма - и каждый следующий прогон берёт
// другие файлы, так что покрытие накапливается.

type DrillOptions struct {
	// MaxBytes ограничивает объём выборки. Ноль - весь снимок.
	MaxBytes int64
	// Dir - куда восстанавливать. Пусто - временный каталог, который
	// удаляется после проверки.
	Dir string
	// Seed фиксирует выборку. Ноль - каждый раз новая, чтобы за несколько
	// прогонов проверилось разное.
	Seed uint64

	Progress func(done, total int64, path string)
	Log      func(level, msg string)
}

func DefaultDrillOptions() DrillOptions {
	return DrillOptions{MaxBytes: 1 << 30}
}

type DrillReport struct {
	SnapshotID   string   `json:"snapshot_id"`
	FilesInSnap  int64    `json:"files_in_snapshot"`
	FilesChecked int64    `json:"files_checked"`
	BytesChecked int64    `json:"bytes_checked"`
	Mismatches   []string `json:"mismatches,omitempty"`
	Problems     []string `json:"problems,omitempty"`
	Duration     string   `json:"duration"`
}

func (d *DrillReport) OK() bool { return len(d.Mismatches) == 0 && len(d.Problems) == 0 }

func (d *DrillReport) Summary() string {
	share := ""
	if d.FilesInSnap > 0 {
		share = fmt.Sprintf(" из %d", d.FilesInSnap)
	}
	if d.OK() {
		return fmt.Sprintf(
			"восстановлено и сверено файлов: %d%s (%s) за %s - снимок пригоден",
			d.FilesChecked, share, repo.HumanBytes(d.BytesChecked), d.Duration)
	}
	return fmt.Sprintf("СНИМОК НЕПРИГОДЕН: расхождений %d, ошибок %d (проверено %d файлов)",
		len(d.Mismatches), len(d.Problems), d.FilesChecked)
}

func (o *DrillOptions) log(level, msg string) {
	if o.Log != nil {
		o.Log(level, msg)
	}
}

// Drill восстанавливает выборку из снимка и сверяет результат.
func Drill(ctx context.Context, r *repo.Repo, snap *repo.Snapshot, opt DrillOptions) (*DrillReport, error) {
	start := time.Now()
	rep := &DrillReport{SnapshotID: snap.ID}

	var files []repo.Node
	if err := r.ReadTree(ctx, snap, func(n *repo.Node) error {
		if n.Type != repo.NodeFile || n.Err != "" {
			return nil
		}
		rep.FilesInSnap++
		files = append(files, *n)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("autobak: дерево снимка не читается: %w", err)
	}
	if len(files) == 0 {
		return nil, errors.New("autobak: в снимке нет файлов - проверять нечего")
	}

	picked, wantBytes := pick(files, opt)
	if len(picked) == 0 {
		return nil, errors.New("autobak: выборка пуста - уменьшите ограничение по объёму")
	}
	opt.log("info", fmt.Sprintf("к проверке отобрано файлов: %d (%s)",
		len(picked), repo.HumanBytes(wantBytes)))

	dir := opt.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "autobak-drill-*")
		if err != nil {
			return nil, err
		}
		// Каталог удаляется всегда: учебное восстановление не должно
		// оставлять после себя копию боевых данных на диске.
		defer os.RemoveAll(tmp)
		dir = tmp
	}

	include := make([]string, 0, len(picked))
	for _, n := range picked {
		include = append(include, n.Path)
	}
	target := NewFS(FSOptions{Root: dir, Overwrite: true, Log: opt.Log})
	if _, err := Run(ctx, r, snap, Options{
		Include: include,
		Log:     opt.Log,
		Progress: func(done, total int64, path string) {
			if opt.Progress != nil {
				opt.Progress(done, total, path)
			}
		},
	}, target); err != nil {
		return rep, fmt.Errorf("autobak: восстановление не выполнилось: %w", err)
	}

	// Сверка: заново режем восстановленный файл и сравниваем имена чанков
	// с записанными в снимке. Имена считались при бэкапе от исходных
	// данных, поэтому совпадение означает побайтовое равенство.
	for _, n := range picked {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		local, err := MapPath(dir, n.Path)
		if err != nil {
			rep.Problems = append(rep.Problems, n.Path+": "+err.Error())
			continue
		}
		if err := compare(r, &n, local); err != nil {
			if errors.Is(err, errMismatch) {
				rep.Mismatches = append(rep.Mismatches, n.Path+": "+err.Error())
			} else {
				rep.Problems = append(rep.Problems, n.Path+": "+err.Error())
			}
			continue
		}
		rep.FilesChecked++
		rep.BytesChecked += n.Size
	}

	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

var errMismatch = errors.New("содержимое не совпало")

func compare(r *repo.Repo, n *repo.Node, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != n.Size {
		return fmt.Errorf("%w: %d байт вместо %d", errMismatch, st.Size(), n.Size)
	}

	c := repo.NewChunker(f, r.Chunker())
	for i := 0; ; i++ {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			if i != len(n.Chunks) {
				return fmt.Errorf("%w: %d чанков вместо %d", errMismatch, i, len(n.Chunks))
			}
			return nil
		}
		if err != nil {
			return err
		}
		if i >= len(n.Chunks) {
			return fmt.Errorf("%w: файл длиннее записанного в снимке", errMismatch)
		}
		if got := r.Key().ChunkID(chunk); got != n.Chunks[i] {
			return fmt.Errorf("%w: чанк %d отличается", errMismatch, i)
		}
	}
}

// pick отбирает случайные файлы в пределах заданного объёма.
//
// Случайно, а не первые попавшиеся: файлы в дереве идут в порядке обхода
// каталогов, и проверка «первого гигабайта» каждый раз щупала бы одни и
// те же файлы одного и того же сайта.
func pick(files []repo.Node, opt DrillOptions) ([]repo.Node, int64) {
	seed := opt.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	rnd := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))

	order := rnd.Perm(len(files))
	var out []repo.Node
	var total int64
	for _, i := range order {
		n := files[i]
		if opt.MaxBytes > 0 && total+n.Size > opt.MaxBytes {
			// Один файл крупнее всего лимита раньше добавлялся целиком
			// (условие len(out)>0 его пропускало), и проверка на 1 ГБ
			// разворачивала во временный каталог, скажем, 500 ГБ, забивая
			// диск. Теперь такой файл просто пропускаем и берём следующий.
			continue
		}
		out = append(out, n)
		total += n.Size
		if opt.MaxBytes > 0 && total >= opt.MaxBytes {
			break
		}
	}
	// Если ни один файл не поместился в лимит (все крупнее), берём самый
	// маленький: проверка обязана хоть что-то восстановить и сверить.
	if len(out) == 0 && len(files) > 0 {
		smallest := files[0]
		for _, n := range files[1:] {
			if n.Size < smallest.Size {
				smallest = n
			}
		}
		out = append(out, smallest)
		total = smallest.Size
	}
	return out, total
}

// DrillDir - куда класть учебное восстановление, если каталог задан явно.
func DrillDir(base, snapshotID string) string {
	return filepath.Join(base, "drill", snapshotID)
}
