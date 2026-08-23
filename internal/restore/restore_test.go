package restore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/engine"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

func buildSite(t *testing.T) (root string, want map[string]string) {
	t.Helper()
	root = t.TempDir()
	want = map[string]string{}
	files := map[string]string{
		"index.php":           "<?php require __DIR__ . '/boot.php';",
		"boot.php":            "<?php define('APP', 1);",
		"assets/style.css":    "body{margin:0}",
		"assets/img/logo.bin": string(bytes.Repeat([]byte{0xAB, 0x01}, 200000)),
		"config/.env":         "DB_PASSWORD=секрет\nAPP_ENV=production",
		"deep/a/b/c/note.txt": "вложенный файл",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		want[rel] = content
	}
	return root, want
}

func backupSite(t *testing.T, root string) (*repo.Repo, *repo.Snapshot) {
	t.Helper()
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir(), backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := repo.Init(ctx, be, "пароль", "")
	if err != nil {
		t.Fatal(err)
	}
	p := plan.New("prod")
	p.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site.ru", Enabled: true,
		Paths: []string{filepath.ToSlash(root)},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "prod", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return r, snap
}

// backupInto делает снимок сайта в уже открытый репозиторий.
func backupInto(t *testing.T, r *repo.Repo, root string) *repo.Snapshot {
	t.Helper()
	p := plan.New("prod")
	p.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site.ru", Enabled: true,
		Paths: []string{filepath.ToSlash(root)},
	}}
	snap, err := engine.Backup(context.Background(), r, engine.Options{
		Plan: p, Server: "prod", Agent: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestRestoreRoundtrip(t *testing.T) {
	ctx := context.Background()
	src, want := buildSite(t)
	r, snap := backupSite(t, src)

	dst := t.TempDir()
	target := NewFS(FSOptions{Root: dst})
	rep, err := Run(ctx, r, snap, Options{}, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())

	// Восстановленное дерево лежит под dst по исходному абсолютному пути.
	base := mustMap(t, dst, src)
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("файл %s не восстановлен: %v", rel, err)
		}
		if string(got) != content {
			t.Fatalf("файл %s восстановлен с искажением (%d байт вместо %d)",
				rel, len(got), len(content))
		}
	}
	if rep.Files != int64(len(want)) {
		t.Fatalf("восстановлено %d файлов вместо %d", rep.Files, len(want))
	}
}

// Без явного разрешения восстановление не должно затирать существующее.
func TestRestoreRefusesOverwriteByDefault(t *testing.T) {
	ctx := context.Background()
	src, _ := buildSite(t)
	r, snap := backupSite(t, src)

	dst := t.TempDir()
	// Первый проход - раскладываем.
	if _, err := Run(ctx, r, snap, Options{}, NewFS(FSOptions{Root: dst})); err != nil {
		t.Fatal(err)
	}
	base := mustMap(t, dst, src)
	victim := filepath.Join(base, "index.php")
	if err := os.WriteFile(victim, []byte("НОВАЯ ВЕРСИЯ"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Второй проход без Overwrite обязан оставить правку на месте.
	if _, err := Run(ctx, r, snap, Options{}, NewFS(FSOptions{Root: dst})); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "НОВАЯ ВЕРСИЯ" {
		t.Fatal("существующий файл был перезаписан без разрешения")
	}

	// С Overwrite - обязан вернуть версию из снимка.
	if _, err := Run(ctx, r, snap, Options{}, NewFS(FSOptions{Root: dst, Overwrite: true})); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(victim)
	if string(got) == "НОВАЯ ВЕРСИЯ" {
		t.Fatal("с разрешением на перезапись файл не восстановился")
	}
}

// Dry-run обязан честно перечислить, что будет затёрто, и ничего не тронуть.
func TestRestoreDryRunListsOverwrites(t *testing.T) {
	ctx := context.Background()
	src, _ := buildSite(t)
	r, snap := backupSite(t, src)

	dst := t.TempDir()
	if _, err := Run(ctx, r, snap, Options{}, NewFS(FSOptions{Root: dst})); err != nil {
		t.Fatal(err)
	}
	before := countFiles(t, dst)

	target := NewFS(FSOptions{Root: dst, Overwrite: true})
	rep, err := Run(ctx, r, snap, Options{DryRun: true}, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Overwrites) == 0 {
		t.Fatal("dry-run не сообщил ни об одной перезаписи")
	}
	if countFiles(t, dst) != before {
		t.Fatal("dry-run изменил файловую систему")
	}
	t.Logf("dry-run: %s", rep.Summary())
}

// Выборочное восстановление одного каталога.
func TestRestoreSubset(t *testing.T) {
	ctx := context.Background()
	src, _ := buildSite(t)
	r, snap := backupSite(t, src)

	only := filepath.ToSlash(filepath.Join(src, "assets"))
	dst := t.TempDir()
	rep, err := Run(ctx, r, snap, Options{Include: []string{only}}, NewFS(FSOptions{Root: dst}))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 2 {
		t.Fatalf("восстановлено %d файлов вместо 2 (%s)", rep.Files, rep.Summary())
	}
	if countFiles(t, dst) != 2 {
		t.Fatalf("на диск легло %d файлов вместо 2", countFiles(t, dst))
	}
}

// Поток восстановления должен доезжать до агента без потерь: это тот же
// путь, которым данные попадают на удалённый сервер.
func TestRestoreOverProtocol(t *testing.T) {
	ctx := context.Background()
	src, want := buildSite(t)
	r, snap := backupSite(t, src)

	var stream bytes.Buffer
	if _, err := Run(ctx, r, snap, Options{}, NewProto(&stream)); err != nil {
		t.Fatal(err)
	}
	t.Logf("поток восстановления: %s", repo.HumanBytes(int64(stream.Len())))

	dst := t.TempDir()
	if err := Apply(ctx, &stream, NewFS(FSOptions{Root: dst})); err != nil {
		t.Fatal(err)
	}
	base := mustMap(t, dst, src)
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("файл %s не доехал: %v", rel, err)
		}
		if string(got) != content {
			t.Fatalf("файл %s искажён при передаче", rel)
		}
	}
}

func TestStripDatabaseStatements(t *testing.T) {
	dump := "-- дамп\n" +
		"CREATE DATABASE IF NOT EXISTS `shop`;\n" +
		"USE `shop`;\n" +
		"CREATE TABLE orders (id INT);\n" +
		"INSERT INTO orders VALUES (1),(2);\n"
	out, err := io.ReadAll(stripDatabaseStatements(bytes.NewReader([]byte(dump))))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if bytes.Contains(out, []byte("CREATE DATABASE")) || bytes.Contains(out, []byte("USE `")) {
		t.Fatalf("переключение базы не вырезано:\n%s", s)
	}
	if !bytes.Contains(out, []byte("CREATE TABLE orders")) ||
		!bytes.Contains(out, []byte("INSERT INTO orders")) {
		t.Fatalf("вырезано лишнее:\n%s", s)
	}
}

func TestDBTargetName(t *testing.T) {
	safe := DBOptions{Mode: DBRestore, Suffix: "_restore_20260821"}
	if got := safe.TargetName("shop"); got != "shop_restore_20260821" {
		t.Fatalf("безопасный режим целится в %q", got)
	}
	if got := (DBOptions{InPlace: true}).TargetName("shop"); got != "shop" {
		t.Fatalf("режим поверх целится в %q", got)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustMap(t *testing.T, root, snapshotPath string) string {
	t.Helper()
	p, err := MapPath(root, filepath.ToSlash(snapshotPath))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Регрессия на находку аудита: восстановление не должно писать «сквозь»
// символическую ссылку за пределы каталога назначения (tar-slip).
func TestRestoreRefusesSymlinkEscape(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("символические ссылки на Windows требуют особых прав")
	}
	root := t.TempDir()
	outside := t.TempDir() // «чужой» каталог за пределами Root

	target := NewFS(FSOptions{Root: root})

	// 1. Враждебный узел-ссылка: escape -> outside.
	link := &repo.Node{Path: "/escape", Type: repo.NodeSymlink, Link: outside}
	if err := target.Node(link, nil); err != nil {
		t.Fatalf("создание ссылки: %v", err)
	}

	// 2. Файл «через» ссылку: /escape/pwned. Должен быть отклонён.
	file := &repo.Node{Path: "/escape/pwned", Type: repo.NodeFile, Mode: 0o644, Size: 4}
	err := target.Node(file, bytes.NewReader([]byte("evil")))
	if err == nil {
		t.Fatal("запись через символическую ссылку не отклонена")
	}

	// Файл не должен появиться в чужом каталоге.
	if _, statErr := os.Stat(filepath.Join(outside, "pwned")); statErr == nil {
		t.Fatal("файл записан за пределы Root через ссылку - tar-slip не закрыт")
	}
}
