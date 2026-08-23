package restore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iamtime/autobak/internal/repo"
)

type FSOptions struct {
	// Root - куда раскладывать. "/" означает восстановление на исходные
	// места; любой другой каталог - безопасный режим, при котором ничего
	// работающего не задевается.
	Root string

	// Overwrite разрешает затирать существующие файлы. Без него
	// восстановление обходит занятые пути стороной и сообщает о них.
	Overwrite bool

	// RestoreOwner возвращает владельца и группу по именам из снимка.
	// Требует root; без него файлы достанутся тому, кто запустил.
	RestoreOwner bool

	// Virtual решает судьбу служебных путей (/@mysql, /@docker).
	// Если nil, они просто раскладываются файлами внутри Root.
	Virtual func(n *repo.Node, content io.Reader) (handled bool, err error)

	Log func(level, msg string)
}

type fsTarget struct {
	opt   FSOptions
	dirs  []*repo.Node
	users *idResolver
	skips []string
}

func NewFS(opt FSOptions) Target {
	if opt.Root == "" {
		opt.Root = "/"
	}
	return &fsTarget{opt: opt, users: newIDResolver()}
}

func (t *fsTarget) logf(level, format string, args ...any) {
	if t.opt.Log != nil {
		t.opt.Log(level, fmt.Sprintf(format, args...))
	}
}

// MapPath переводит путь из снимка в путь на диске.
//
// Экспортирована, потому что интерфейс обязан показать человеку, куда
// именно лягут данные, до того как он нажмёт «Восстановить», - и показать
// ровно то, что сделает восстановление, а не похожее на него.
//
// Пути приходят из репозитория, то есть из данных, подпись которых мы
// проверили. Но подпись означает лишь «это писали мы», а не «это
// безопасно»: снимок мог быть снят с сервера, где кто-то заранее создал
// путь с переходом вверх. Поэтому выход за root отсекается здесь.
func MapPath(root, p string) (string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("небезопасный путь %q", p)
	}
	if root == "" || root == "/" {
		return filepath.FromSlash(clean), nil
	}
	// На Windows двоеточие из пути вида C:/... сломало бы имя файла.
	if os.PathSeparator == '\\' {
		clean = strings.ReplaceAll(clean, ":", "_")
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/"))), nil
}

func (t *fsTarget) localPath(p string) (string, error) { return MapPath(t.opt.Root, p) }

// safeParents проверяет, что ни один существующий каталог на пути к dst не
// является символической ссылкой.
//
// Это защита от классической атаки tar-slip: враждебный снимок кладёт
// узел-ссылку "a" → "/etc", а следом файл "a/passwd". MapPath держит
// логический путь внутри Root, но операционная система, открывая
// "Root/a/passwd", пройдёт по ссылке наружу, и файл окажется в /etc.
// MkdirAll и CreateTemp следуют по ссылкам, os.Lstat проверяет только
// последний компонент. Поэтому проверяем всю цепочку родителей.
//
// В обычном бэкапе такого не бывает: если каталог - ссылка, его
// содержимое не сохраняется отдельными узлами под ним. Значит отказ
// ломает только атаку, а не честное восстановление.
func (t *fsTarget) safeParents(dst string) error {
	root := t.opt.Root
	if root == "" {
		root = string(os.PathSeparator)
	}
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, filepath.Dir(dst))
	if err != nil {
		return err
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // дальше пути ещё нет - создадим сами, ссылок там нет
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("autobak: отказ писать через символическую ссылку %q "+
				"(защита от выхода за пределы каталога восстановления)", cur)
		}
	}
	return nil
}

func (t *fsTarget) WouldOverwrite(n *repo.Node) bool {
	if n.Type == repo.NodeDir {
		return false
	}
	p, err := t.localPath(n.Path)
	if err != nil {
		return false
	}
	_, err = os.Lstat(p)
	return err == nil
}

func (t *fsTarget) Node(n *repo.Node, content io.Reader) error {
	if t.opt.Virtual != nil && strings.HasPrefix(n.Path, "/@") {
		handled, err := t.opt.Virtual(n, content)
		if err != nil || handled {
			return err
		}
	}

	dst, err := t.localPath(n.Path)
	if err != nil {
		return err
	}
	// Ни один существующий родитель пути не должен быть символической
	// ссылкой: иначе запись «сквозь» неё вышла бы за пределы Root.
	if err := t.safeParents(dst); err != nil {
		return err
	}

	switch n.Type {
	case repo.NodeDir:
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		// Права и время каталога выставляются в самом конце: пока внутрь
		// пишутся файлы, каталог должен оставаться доступным на запись,
		// а его mtime всё равно обновится при каждой записи.
		t.dirs = append(t.dirs, n)
		return nil

	case repo.NodeSymlink:
		if err := t.prepare(dst, n); err != nil {
			// Существующая ссылка при запрете перезаписи - это пропуск
			// одного объекта, а не повод обрывать всё восстановление.
			if errors.Is(err, errSkip) {
				t.logf("warn", "%s уже существует - пропущено", n.Path)
				return nil
			}
			return err
		}
		if err := os.Symlink(filepath.FromSlash(n.Link), dst); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil
			}
			// Windows без прав разработчика не умеет символические ссылки;
			// терять из-за этого весь restore не стоит.
			t.logf("warn", "ссылка %s не создана: %v", n.Path, err)
			return nil
		}
		return applyOwner(dst, n, t.users, t.opt.RestoreOwner)

	case repo.NodeFile:
		return t.writeFile(dst, n, content)

	default:
		// Сокеты, каналы и устройства: воссоздаём, если умеем и если мы root.
		if err := t.prepare(dst, n); err != nil {
			if errors.Is(err, errSkip) {
				t.logf("warn", "%s уже существует - пропущено", n.Path)
				return nil
			}
			return err
		}
		if err := makeSpecial(dst, n, t.opt.RestoreOwner); err != nil {
			t.logf("warn", "%s не воссоздан: %v", n.Path, err)
		}
		return nil
	}
}

// prepare разбирается с уже существующим объектом по пути назначения.
func (t *fsTarget) prepare(dst string, n *repo.Node) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !t.opt.Overwrite {
		t.skips = append(t.skips, n.Path)
		return errSkip
	}
	return os.RemoveAll(dst)
}

var errSkip = errors.New("пропущено: файл существует")

func (t *fsTarget) writeFile(dst string, n *repo.Node, content io.Reader) error {
	if err := t.prepare(dst, n); err != nil {
		if errors.Is(err, errSkip) {
			// Содержимое обязано быть вычитано даже при пропуске: иначе
			// в потоковом режиме следующий файл начнёт читать чужие байты.
			_, _ = io.Copy(io.Discard, content)
			t.logf("warn", "%s уже существует - пропущен", n.Path)
			return nil
		}
		return err
	}

	// Пишем во временный файл рядом и переименовываем: прерванное
	// восстановление не оставит наполовину записанный index.php,
	// который сайт немедленно начнёт отдавать посетителям.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".autobak-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	written, err := io.Copy(tmp, content)
	if err != nil {
		return err
	}
	if written != n.Size {
		return fmt.Errorf("восстановлено %d байт вместо %d", written, n.Size)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, os.FileMode(n.Mode&0o777)); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	applyModeBits(dst, n, t.opt.RestoreOwner)
	if err := applyOwner(dst, n, t.users, t.opt.RestoreOwner); err != nil {
		t.logf("warn", "владелец %s не восстановлен: %v", n.Path, err)
	}
	applyXattrs(dst, n)
	if n.MTime != 0 {
		mt := time.Unix(0, n.MTime)
		_ = os.Chtimes(dst, mt, mt)
	}
	return nil
}

// Finish доводит каталоги: их права, владельца и время можно выставлять
// только после того, как всё содержимое записано.
func (t *fsTarget) Finish() error {
	// От глубоких к мелким: иначе снятие права записи с родителя помешает
	// дописать вложенные каталоги.
	sort.Slice(t.dirs, func(i, j int) bool {
		return len(t.dirs[i].Path) > len(t.dirs[j].Path)
	})
	for _, n := range t.dirs {
		dst, err := t.localPath(n.Path)
		if err != nil {
			continue
		}
		_ = os.Chmod(dst, os.FileMode(n.Mode&0o777))
		applyModeBits(dst, n, t.opt.RestoreOwner)
		if err := applyOwner(dst, n, t.users, t.opt.RestoreOwner); err != nil {
			t.logf("warn", "владелец каталога %s не восстановлен: %v", n.Path, err)
		}
		applyXattrs(dst, n)
		if n.MTime != 0 {
			mt := time.Unix(0, n.MTime)
			_ = os.Chtimes(dst, mt, mt)
		}
	}
	if len(t.skips) > 0 {
		t.logf("warn", "пропущено существующих объектов: %d", len(t.skips))
	}
	return nil
}

// Skipped - что не тронули из-за запрета перезаписи.
func (t *fsTarget) Skipped() []string { return t.skips }
