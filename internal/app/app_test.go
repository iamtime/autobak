package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/sshx"
)

// buildAgent собирает настоящий бинарь агента.
//
// Тест намеренно идёт через него, а не через вызов пакетов напрямую:
// проверяется вся цепочка целиком - разбор плана, кадры протокола,
// коды возврата процесса. Всё, что тестируется в обход процесса,
// имеет привычку ломаться именно в процессе.
func buildAgent(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("нет компилятора go - сквозной тест пропущен")
	}
	name := "autobak-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "github.com/iamtime/autobak/cmd/autobak-agent")
	if res, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("не собрать агента: %v\n%s", err, res)
	}
	return out
}

func makeSite(t *testing.T) (dir string, files map[string]string) {
	t.Helper()
	dir = t.TempDir()
	files = map[string]string{
		"public_html/index.php":     "<?php echo 'привет';",
		"public_html/config.php":    "<?php $db = 'shop';",
		"public_html/assets/app.js": strings.Repeat("console.log(1);\n", 5000),
		"public_html/uploads/a.bin": strings.Repeat("\x01\x02\x03\x04", 100000),
		"public_html/debug.log":     "это должно быть исключено",
		"private/notes.txt":         "заметки",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	delete(files, "public_html/debug.log") // ожидаем, что он не попадёт в снимок
	return dir, files
}

func setup(t *testing.T) (*App, *Server, string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	agent := buildAgent(t)
	site, want := makeSite(t)

	a, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cr := &Repo{Name: "тест", Kind: RepoLocal, Path: t.TempDir()}
	recovery, err := a.AddRepo(ctx, cr, "", "пароль-репозитория")
	if err != nil {
		t.Fatal(err)
	}
	if recovery == "" {
		t.Fatal("создание репозитория не вернуло recovery-код")
	}

	s := &Server{
		Name:   "prod",
		SSH:    sshx.Target{Local: true, AgentPath: agent},
		RepoID: cr.ID,
		Mode:   ModePull,
	}
	if err := a.AddServer(s); err != nil {
		t.Fatal(err)
	}
	s.Plan = *plan.New("prod")
	s.Plan.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site.ru", Enabled: true,
		Paths: []string{filepath.ToSlash(site)},
	}}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	return a, s, site, want
}

// Полный путь: агент отдаёт данные процессом, десктоп их шифрует и
// складывает, затем читает обратно и раскладывает на диск.
func TestEndToEndBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	a, s, site, want := setup(t)

	var logs []string
	ev := Events{Log: func(level, msg string) { logs = append(logs, level+": "+msg) }}

	snap, err := a.Backup(ctx, s.ID, ev)
	if err != nil {
		t.Fatalf("бэкап не прошёл: %v\nжурнал: %s", err, strings.Join(logs, "\n"))
	}
	if !snap.Complete() {
		t.Fatalf("снимок неполон: %+v", snap.Failed())
	}
	if snap.Stats.Files != int64(len(want)) {
		t.Fatalf("в снимке %d файлов, ожидалось %d", snap.Stats.Files, len(want))
	}
	t.Logf("снимок %s: файлов %d, прочитано %s, в хранилище %s",
		snap.ID, snap.Stats.Files,
		repo.HumanBytes(snap.Stats.BytesTotal), repo.HumanBytes(snap.Stats.BytesStored))

	// Состояние сервера обязано обновиться - по нему интерфейс рисует статус.
	if !s.Last.OK || s.Last.SnapshotID != snap.ID {
		t.Fatalf("итог последнего запуска не сохранён: %+v", s.Last)
	}

	snaps, err := a.Snapshots(ctx, s.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("список снимков: %v, %d штук", err, len(snaps))
	}

	// Восстановление на этот компьютер.
	dst := t.TempDir()
	rep, err := a.Restore(ctx, s.ID, RestoreOptions{
		SnapshotID: snap.ID, LocalDir: dst,
	}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != int64(len(want)) {
		t.Fatalf("восстановлено %d файлов вместо %d", rep.Files, len(want))
	}

	base, err := restore.MapPath(dst, filepath.ToSlash(site))
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("файл %s не восстановлен: %v", rel, err)
		}
		if string(got) != content {
			t.Fatalf("файл %s восстановлен с искажением", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "public_html", "debug.log")); err == nil {
		t.Fatal("исключённый файл журнала попал в бэкап")
	}
}

// Второй бэкап без изменений почти ничего не должен добавить -
// иначе хранилище будет расти как при полных копиях каждый день.
func TestIncrementalIsCheap(t *testing.T) {
	ctx := context.Background()
	a, s, site, _ := setup(t)

	first, err := a.Backup(ctx, s.ID, Events{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Backup(ctx, s.ID, Events{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Stats.BytesNew > first.Stats.BytesNew/100 {
		t.Fatalf("повторный бэкап без изменений записал %s при первых %s",
			repo.HumanBytes(second.Stats.BytesNew), repo.HumanBytes(first.Stats.BytesNew))
	}

	// А правка одного файла - ровно свою цену, а не цену всего сайта.
	target := filepath.Join(site, "public_html", "config.php")
	if err := os.WriteFile(target, []byte("<?php $db = 'shop_v2';"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := a.Backup(ctx, s.ID, Events{})
	if err != nil {
		t.Fatal(err)
	}
	if third.Stats.BytesNew > first.Stats.BytesNew/10 {
		t.Fatalf("правка одного файла стоила %s при полном объёме %s",
			repo.HumanBytes(third.Stats.BytesNew), repo.HumanBytes(first.Stats.BytesTotal))
	}
	t.Logf("первый %s → второй %s → после правки %s",
		repo.HumanBytes(first.Stats.BytesNew),
		repo.HumanBytes(second.Stats.BytesNew),
		repo.HumanBytes(third.Stats.BytesNew))
}

// Политика хранения и сборка мусора на реальных снимках.
func TestPruneKeepsLatest(t *testing.T) {
	ctx := context.Background()
	a, s, site, _ := setup(t)

	for i := range 3 {
		if err := os.WriteFile(filepath.Join(site, "private", "notes.txt"),
			[]byte(fmt.Sprintf("версия %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Backup(ctx, s.ID, Events{}); err != nil {
			t.Fatal(err)
		}
	}
	s.Retention = repo.Retention{Last: 1}

	dry, err := a.Prune(ctx, s.ID, true, Events{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.SnapshotsRemoved) != 2 {
		t.Fatalf("сухой прогон насчитал %d снимков к удалению вместо 2", len(dry.SnapshotsRemoved))
	}
	if _, err := a.Prune(ctx, s.ID, false, Events{}); err != nil {
		t.Fatal(err)
	}
	snaps, err := a.Snapshots(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("после очистки осталось %d снимков вместо 1", len(snaps))
	}

	// И оставшийся обязан по-прежнему восстанавливаться.
	rep, err := a.Restore(ctx, s.ID, RestoreOptions{
		SnapshotID: snaps[0].ID, LocalDir: t.TempDir(),
	}, Events{})
	if err != nil {
		t.Fatalf("оставшийся снимок не восстанавливается: %v", err)
	}
	if rep.Files == 0 {
		t.Fatal("восстановление вернуло ноль файлов")
	}
}

// Обследование через настоящий процесс агента.
func TestDiscoverThroughAgent(t *testing.T) {
	ctx := context.Background()
	a, s, _, _ := setup(t)

	rep, err := a.Discover(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Agent == "" {
		t.Fatal("агент не сообщил свою версию")
	}
	t.Logf("обследование: %s", rep.Summary())
}
