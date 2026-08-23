package collect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

// filesCollector обходит каталоги и отдаёт их содержимое.
//
// Обход собственный, а не filepath.WalkDir, по трём причинам: нужно
// отсекать поддерево целиком по одному совпадению исключения, нужно уметь
// не пересекать границы файловых систем и нужно, чтобы ошибка на одном
// файле не обрывала обход. Последнее принципиально: бэкап, падающий из-за
// файла, который PHP удалил у нас из-под ног, бесполезен.
type filesCollector struct {
	m       plan.Module
	match   *Matcher
	users   *userCache
	maxSize int64

	// strip и prefix переписывают путь узла. Нужны сборщику docker: тома
	// физически лежат в /var/lib/docker/volumes/<имя>/_data, но в снимке
	// обязаны выглядеть как /@docker/volumes/<имя>, иначе восстановление
	// на сервер с другим расположением докера промахнётся мимо цели.
	strip, prefix string

	// visited хранит уже пройденные каталоги при переходе по ссылкам.
	// Без него две ссылки друг на друга увели бы обход в бесконечность.
	visited map[fileKey]bool
	// follows считает переходы по ссылкам. На Linux петли ловит visited по
	// inode; но keyOf вне Linux заглушка (inode подделать нечем), и там
	// петля симлинков ушла бы в бесконечную рекурсию. Общий потолок на
	// число переходов - грубая, но платформонезависимая страховка: у
	// честного бэкапа переходов на порядки меньше.
	follows int

	files, dirs, skipped, failed int64
}

// maxSymlinkFollows - потолок переходов по ссылкам за весь бэкап.
const maxSymlinkFollows = 1_000_000

func newFiles(m plan.Module, global []string) *filesCollector {
	pats := append(append([]string{}, global...), m.Excludes...)
	return &filesCollector{m: m, match: NewMatcher(pats...), users: newUserCache()}
}

func (c *filesCollector) Kind() plan.Kind { return c.m.Kind }
func (c *filesCollector) Name() string    { return c.m.Name }

// SetMaxFileSize задаёт потолок размера файла (0 - без ограничения).
func (c *filesCollector) SetMaxFileSize(n int64) { c.maxSize = n }

func (c *filesCollector) Collect(ctx context.Context, s Sink) (map[string]any, error) {
	roots := append([]string(nil), c.m.Paths...)
	sort.Strings(roots) // порядок обхода фиксирован - дерево лучше дедуплицируется

	c.visited = map[fileKey]bool{}

	var missing []string
	for _, root := range roots {
		root = path.Clean(root)
		// По корню идём с разыменованием ссылки, а не Lstat.
		//
		// Типовая схема с релизами делает public_html ссылкой на
		// /srv/releases/42. При Lstat в снимок попала бы одна ссылка,
		// и сайт оказался бы не сохранён вовсе - при этом бэкап
		// выглядел бы успешным.
		fi, err := os.Stat(filepath.FromSlash(root))
		if errors.Is(err, fs.ErrNotExist) {
			// Набор путей приходит из автообнаружения и может содержать то,
			// чего на этом сервере нет. Это не ошибка модуля.
			missing = append(missing, root)
			s.Logf("info", "путь %s отсутствует - пропущен", root)
			continue
		}
		if err != nil {
			c.failed++
			s.Logf("warn", "%s: %v", root, err)
			continue
		}
		if err := c.walk(ctx, s, root, fi, deviceOf(fi)); err != nil {
			return c.meta(missing), err
		}
	}
	return c.meta(missing), nil
}

func (c *filesCollector) meta(missing []string) map[string]any {
	m := map[string]any{
		"paths":   c.m.Paths,
		"dirs":    c.dirs,
		"skipped": c.skipped,
	}
	if len(missing) > 0 {
		m["missing"] = missing
	}
	if c.failed > 0 {
		m["failed"] = c.failed
	}
	return m
}

func (c *filesCollector) walk(ctx context.Context, s Sink, p string, fi os.FileInfo, rootDev uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.match.Match(p) {
		c.skipped++
		return nil
	}

	n := c.node(p, fi)

	switch n.Type {
	case repo.NodeDir:
		if c.m.OneFilesystem && deviceOf(fi) != rootDev {
			s.Logf("info", "%s - другая файловая система, пропущено", p)
			c.skipped++
			return nil
		}
		c.dirs++
		if err := s.Meta(n); err != nil {
			return err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			// Каталог без прав на чтение отмечается в дереве и не роняет обход:
			// при восстановлении будет видно, что здесь чего-то не хватает.
			c.failed++
			s.Logf("warn", "%s: %v", p, err)
			n.Err = err.Error()
			return s.Meta(n)
		}
		for _, e := range entries {
			sub := path.Join(p, e.Name())
			sfi, err := os.Lstat(filepath.FromSlash(sub))
			if err != nil {
				// Файл исчез между чтением каталога и lstat - обычное дело
				// на живом сервере, тревожить этим пользователя незачем.
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				c.failed++
				s.Logf("warn", "%s: %v", sub, err)
				continue
			}
			if err := c.walk(ctx, s, sub, sfi, rootDev); err != nil {
				return err
			}
		}
		return nil

	case repo.NodeSymlink:
		target, err := os.Readlink(filepath.FromSlash(p))
		if err != nil {
			c.failed++
			n.Err = err.Error()
		}
		n.Link = target
		if !c.m.FollowSymlinks {
			return s.Meta(n)
		}
		// Переход по ссылке: сохраняем и саму ссылку, и то, куда она
		// ведёт. Иначе при восстановлении ссылка осталась бы висеть
		// в пустоту.
		if err := s.Meta(n); err != nil {
			return err
		}
		resolved, err := os.Stat(filepath.FromSlash(p))
		if err != nil {
			s.Logf("warn", "ссылка %s ведёт в никуда: %v", p, err)
			return nil
		}
		if k, ok := keyOf(resolved); ok {
			if c.visited[k] {
				s.Logf("info", "%s уже обойдено по другой ссылке - пропущено", p)
				return nil
			}
			c.visited[k] = true
		}
		c.follows++
		if c.follows > maxSymlinkFollows {
			return fmt.Errorf("autobak: слишком много переходов по ссылкам (>%d) - возможна петля симлинков",
				maxSymlinkFollows)
		}
		return c.walkResolved(ctx, s, p, resolved, rootDev)

	case repo.NodeFile:
		if c.maxSize > 0 && fi.Size() > c.maxSize {
			c.skipped++
			s.Logf("warn", "%s (%s) больше лимита - пропущен", p, repo.HumanBytes(fi.Size()))
			return nil
		}
		f, err := os.Open(filepath.FromSlash(p))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			c.failed++
			s.Logf("warn", "%s: %v", p, err)
			n.Err = err.Error()
			return s.Meta(n)
		}
		defer f.Close()
		c.files++
		s.Progress(p, 0)
		return s.File(n, f)

	default:
		// Сокеты, каналы и устройства сохраняются только метаданными:
		// содержимого у них нет, а воссоздать их при restore нужно.
		return s.Meta(n)
	}
}

// walkResolved обходит то, на что указывает ссылка, оставляя пути
// такими, какими их видит пользователь: содержимое /srv/releases/42
// попадёт в снимок под путём ссылки, а не цели.
func (c *filesCollector) walkResolved(ctx context.Context, s Sink, p string, fi os.FileInfo, rootDev uint64) error {
	if !fi.IsDir() {
		f, err := os.Open(filepath.FromSlash(p))
		if err != nil {
			c.failed++
			s.Logf("warn", "%s: %v", p, err)
			return nil
		}
		defer f.Close()
		n := c.node(p, fi)
		n.Type = repo.NodeFile
		c.files++
		return s.File(n, f)
	}
	entries, err := os.ReadDir(filepath.FromSlash(p))
	if err != nil {
		c.failed++
		s.Logf("warn", "%s: %v", p, err)
		return nil
	}
	for _, e := range entries {
		sub := path.Join(p, e.Name())
		sfi, err := os.Lstat(filepath.FromSlash(sub))
		if err != nil {
			continue
		}
		if err := c.walk(ctx, s, sub, sfi, rootDev); err != nil {
			return err
		}
	}
	return nil
}

func (c *filesCollector) nodePath(p string) string {
	if c.strip == "" {
		return p
	}
	// path.Join, а не конкатенация: strip может как оканчиваться слэшем,
	// так и нет, и склейка вслепую давала бы /@docker/composeopt/stack.
	return path.Join(c.prefix, strings.TrimPrefix(p, c.strip))
}

func (c *filesCollector) node(p string, fi os.FileInfo) *repo.Node {
	n := &repo.Node{
		Path:   c.nodePath(p),
		Module: c.m.Name,
		Mode:   uint32(fi.Mode().Perm()),
		Size:   fi.Size(),
		MTime:  fi.ModTime().UnixNano(),
	}
	mode := fi.Mode()
	switch {
	case mode.IsDir():
		n.Type = repo.NodeDir
		n.Size = 0
	case mode&os.ModeSymlink != 0:
		n.Type = repo.NodeSymlink
		n.Size = 0
	case mode&os.ModeSocket != 0:
		n.Type = repo.NodeSocket
	case mode&os.ModeNamedPipe != 0:
		n.Type = repo.NodeFIFO
	case mode&os.ModeCharDevice != 0:
		n.Type = repo.NodeChar
	case mode&os.ModeDevice != 0:
		n.Type = repo.NodeDevice
	default:
		n.Type = repo.NodeFile
	}
	// setuid, setgid и sticky не входят в Perm(), но потерять их нельзя:
	// без setgid на каталоге сломаются права у всего, что в нём создаётся.
	if mode&os.ModeSetuid != 0 {
		n.Mode |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		n.Mode |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		n.Mode |= 0o1000
	}
	fillSysMeta(n, filepath.FromSlash(p), fi, c.users)
	return n
}

// countingReader сообщает Sink о прогрессе по мере чтения крупного файла,
// чтобы полоса не замирала на многогигабайтном архиве.
type countingReader struct {
	r    io.Reader
	path string
	s    Sink
	acc  int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.acc += int64(n)
	if cr.acc >= 8<<20 {
		cr.s.Progress(cr.path, cr.acc)
		cr.acc = 0
	}
	return n, err
}
