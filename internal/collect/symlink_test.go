package collect

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iamtime/autobak/internal/plan"
)

func symlinksSupported(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(dir, "probe")); err != nil {
		t.Skipf("символические ссылки недоступны (%s): %v", runtime.GOOS, err)
	}
}

// Типовая схема с релизами: корень сайта - ссылка на каталог релиза.
// Раньше в снимок попадала одна ссылка, и сайт оказывался не сохранён,
// притом что бэкап выглядел успешным.
func TestFilesCollectorFollowsRootSymlink(t *testing.T) {
	symlinksSupported(t)
	base := t.TempDir()

	release := filepath.Join(base, "releases", "42")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "index.php"), []byte("<?php // релиз 42"), 0o644); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(base, "current")
	if err := os.Symlink(release, current); err != nil {
		t.Fatal(err)
	}

	c := newFiles(plan.Module{
		Kind: plan.KindFiles, Name: "site", Enabled: true,
		Paths: []string{filepath.ToSlash(current)},
	}, nil)
	s := newTestSink()
	if _, err := c.Collect(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	all := strings.Join(s.paths(), "\n")
	if !strings.Contains(all, "index.php") {
		t.Fatalf("содержимое релиза не попало в бэкап:\n%s", all)
	}
	// Пути должны быть такими, какими их видит человек: через ссылку,
	// а не через настоящее расположение релиза.
	want := filepath.ToSlash(filepath.Join(current, "index.php"))
	if s.data[want] != "<?php // релиз 42" {
		t.Fatalf("файл лежит не под путём ссылки (собрано: %v)", s.paths())
	}
}

// Переход по вложенным ссылкам включается отдельно и не должен уводить
// обход в бесконечность, если ссылки образуют петлю.
func TestFilesCollectorSymlinkLoop(t *testing.T) {
	symlinksSupported(t)
	if runtime.GOOS != "linux" {
		t.Skip("защита от петель опирается на инод и работает на Linux")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("данные"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ссылка внутрь себя: без защиты обход не закончится никогда.
	if err := os.Symlink(root, filepath.Join(sub, "loop")); err != nil {
		t.Fatal(err)
	}

	c := newFiles(plan.Module{
		Kind: plan.KindFiles, Name: "site", Enabled: true,
		Paths: []string{filepath.ToSlash(root)}, FollowSymlinks: true,
	}, nil)
	s := newTestSink()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Collect(ctx, s)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("обход не завершился - защита от петель не сработала")
	}
	if len(s.data) == 0 {
		t.Fatal("не собрано ни одного файла")
	}
}
