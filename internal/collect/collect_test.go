package collect

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

func TestMatcher(t *testing.T) {
	m := NewMatcher(plan.DefaultExcludes()...)

	skip := []string{
		"/home/admin/web/site.ru/node_modules",
		"/home/admin/web/site.ru/node_modules/react/index.js",
		"/var/www/app/storage/logs/laravel.log",
		"/var/www/app/bitrix/cache/x/y.php",
		"/tmp/app.log",
		"/run/php/php8.2-fpm.sock",
		"/var/run/nginx.pid",
	}
	for _, p := range skip {
		if !m.Match(p) {
			t.Errorf("должен исключаться, но не исключён: %s", p)
		}
	}

	keep := []string{
		"/home/admin/web/site.ru/index.php",
		"/home/admin/web/site.ru/vendor/autoload.php",
		"/etc/nginx/nginx.conf",
		// Похоже на node_modules, но это другой каталог - граница сегмента
		// обязана соблюдаться.
		"/home/admin/my_node_modules/file.js",
		"/home/admin/logs.txt",
	}
	for _, p := range keep {
		if m.Match(p) {
			t.Errorf("не должен исключаться, но исключён: %s", p)
		}
	}
}

func TestMatcherForms(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.log", "/a/b/c.log", true},
		{"*.log", "/a/b/c.txt", false},
		{"**/cache", "/a/cache/b", true},
		{"**/cache", "/a/nocache/b", false},
		{"/var/log/**", "/var/log/nginx/access", true},
		{"/var/log/**", "/var/logs/nginx", false},
		{"/etc/passwd", "/etc/passwd", true},
		{"storage/logs", "/app/storage/logs/x.log", true},
		{"storage/logs", "/app/mystorage/logs/x.log", false},
	}
	for _, c := range cases {
		if got := NewMatcher(c.pattern).Match(c.path); got != c.want {
			t.Errorf("шаблон %q против %q: получено %v, ожидалось %v",
				c.pattern, c.path, got, c.want)
		}
	}
}

// testSink собирает всё, что отдал сборщик, чтобы проверить результат.
type testSink struct {
	nodes []*repo.Node
	data  map[string]string
	logs  []string
}

func newTestSink() *testSink { return &testSink{data: map[string]string{}} }

func (s *testSink) Meta(n *repo.Node) error {
	c := *n
	s.nodes = append(s.nodes, &c)
	return nil
}

func (s *testSink) File(n *repo.Node, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c := *n
	c.Size = int64(len(b))
	s.nodes = append(s.nodes, &c)
	s.data[c.Path] = string(b)
	return nil
}

func (s *testSink) Logf(level, format string, args ...any) {
	s.logs = append(s.logs, level)
}
func (s *testSink) Progress(string, int64) {}

func (s *testSink) paths() []string {
	var out []string
	for _, n := range s.nodes {
		out = append(out, n.Path)
	}
	return out
}

func TestFilesCollectorWalksAndExcludes(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("public_html/index.php", "<?php echo 1;")
	mk("public_html/app.log", "мусор")
	mk("public_html/node_modules/react/index.js", "big")
	mk("storage/data.txt", "важное")

	slashRoot := filepath.ToSlash(root)
	m := plan.Module{Kind: plan.KindFiles, Name: "site", Enabled: true, Paths: []string{slashRoot}}
	c := newFiles(m, plan.DefaultExcludes())

	s := newTestSink()
	meta, err := c.Collect(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}

	all := strings.Join(s.paths(), "\n")
	if !strings.Contains(all, "public_html/index.php") {
		t.Fatalf("index.php не попал в бэкап:\n%s", all)
	}
	if !strings.Contains(all, "storage/data.txt") {
		t.Fatalf("data.txt не попал в бэкап:\n%s", all)
	}
	if strings.Contains(all, "app.log") {
		t.Fatal("файл .log должен был исключиться")
	}
	if strings.Contains(all, "node_modules") {
		t.Fatal("node_modules должен был исключиться целиком")
	}
	if s.data[slashRoot+"/public_html/index.php"] != "<?php echo 1;" {
		t.Fatalf("содержимое файла потеряно: %q", s.data[slashRoot+"/public_html/index.php"])
	}
	if meta["skipped"].(int64) < 2 {
		t.Fatalf("счётчик пропущенных неверен: %v", meta["skipped"])
	}
}

// Переписывание путей нужно докеру: том лежит в /var/lib/docker/..., а в
// снимке обязан быть под /@docker/volumes/<имя>.
func TestFilesCollectorPathRewrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dump.rdb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	slashRoot := filepath.ToSlash(root)

	c := newFiles(plan.Module{Kind: plan.KindFiles, Name: "docker", Paths: []string{slashRoot}}, nil)
	c.strip = slashRoot
	c.prefix = "/@docker/volumes/redis-data"

	s := newTestSink()
	if _, err := c.Collect(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"/@docker/volumes/redis-data":          true,
		"/@docker/volumes/redis-data/dump.rdb": true,
	}
	for _, p := range s.paths() {
		if !want[p] {
			t.Fatalf("неожиданный путь после переписывания: %q (все: %v)", p, s.paths())
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("не выданы пути: %v", want)
	}
}

// Файл, который нельзя прочитать, обязан быть отмечен в дереве, но не
// обрывать обход: на живом сервере такое происходит постоянно.
func TestFilesCollectorSurvivesUnreadable(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(root, "gone.txt")
	if err := os.WriteFile(missing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}

	c := newFiles(plan.Module{Kind: plan.KindFiles, Name: "t",
		Paths: []string{filepath.ToSlash(root)}}, nil)
	s := newTestSink()
	if _, err := c.Collect(context.Background(), s); err != nil {
		t.Fatalf("обход прерван из-за одного файла: %v", err)
	}
	if len(s.data) != 3 {
		t.Fatalf("собрано %d файлов вместо 3", len(s.data))
	}
}
