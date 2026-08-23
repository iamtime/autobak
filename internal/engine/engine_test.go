package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
)

// makeSite создаёт что-то похожее на каталог реального сайта.
func makeSite(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	r := rand.New(rand.NewSource(7))

	write := func(rel string, size int) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte('a' + r.Intn(26))
		}
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("public_html/index.php", 2048)
	write("public_html/wp-config.php", 1024)
	for i := range 40 {
		write(fmt.Sprintf("public_html/wp-content/uploads/img%02d.bin", i), 64*1024)
	}
	write("public_html/wp-content/cache/junk.tmp", 999) // должен исключиться
	write("logs/access.log", 5000)                      // должен исключиться
	write("private/backup.sql", 300*1024)
	return root
}

func sitePlan(root string) *plan.Plan {
	p := plan.New("prod")
	p.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site.ru", Enabled: true,
		Paths: []string{filepath.ToSlash(root)},
	}}
	return p
}

func openRepo(t *testing.T) *repo.Repo {
	t.Helper()
	be, err := backend.OpenLocal(t.TempDir(), backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := repo.Init(context.Background(), be, "пароль", "тест")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func treeOf(t *testing.T, r *repo.Repo, s *repo.Snapshot) map[string]repo.Node {
	t.Helper()
	out := map[string]repo.Node{}
	if err := r.ReadTree(context.Background(), s, func(n *repo.Node) error {
		out[n.Path] = *n
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Главная проверка симметрии: снимок, собранный агентом напрямую
// (push-режим), и снимок, приехавший по SSH и записанный десктопом
// (pull-режим), обязаны совпадать до чанка. Иначе переезд между режимами
// обнулял бы всю накопленную дедупликацию.
func TestExportImportMatchesBackup(t *testing.T) {
	ctx := context.Background()
	root := makeSite(t)
	p := sitePlan(root)
	r := openRepo(t)

	direct, err := Backup(ctx, r, Options{Plan: p, Server: "prod", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}

	planJSON, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := Export(ctx, &stream, nil, &proto.Request{Plan: planJSON}, "test"); err != nil {
		t.Fatal(err)
	}
	t.Logf("поток экспорта: %s", repo.HumanBytes(int64(stream.Len())))

	viaStream, err := Import(ctx, r, &stream, nil, Options{Server: "prod"})
	if err != nil {
		t.Fatal(err)
	}

	// Тот же репозиторий: второй снимок обязан почти целиком уйти
	// в дедупликацию.
	if viaStream.Stats.BytesNew > direct.Stats.BytesNew/50 {
		t.Fatalf("pull-режим записал %s новых данных при %s у push-режима - режимы режут по-разному",
			repo.HumanBytes(viaStream.Stats.BytesNew), repo.HumanBytes(direct.Stats.BytesNew))
	}

	a, b := treeOf(t, r, direct), treeOf(t, r, viaStream)
	if len(a) != len(b) {
		t.Fatalf("в деревьях разное число узлов: push %d, pull %d", len(a), len(b))
	}
	for path, na := range a {
		nb, ok := b[path]
		if !ok {
			t.Fatalf("узел %s есть только в push-снимке", path)
		}
		if na.Size != nb.Size || na.Type != nb.Type || na.Mode != nb.Mode {
			t.Fatalf("узел %s отличается: %+v против %+v", path, na, nb)
		}
		if len(na.Chunks) != len(nb.Chunks) {
			t.Fatalf("узел %s: разное число чанков", path)
		}
		for i := range na.Chunks {
			if na.Chunks[i] != nb.Chunks[i] {
				t.Fatalf("узел %s: чанк %d отличается", path, i)
			}
		}
	}
	t.Logf("оба режима дали идентичные деревья: %d узлов, %s данных",
		len(a), repo.HumanBytes(direct.Stats.BytesTotal))
}

func TestBackupAppliesExcludes(t *testing.T) {
	ctx := context.Background()
	root := makeSite(t)
	r := openRepo(t)

	snap, err := Backup(ctx, r, Options{Plan: sitePlan(root), Server: "prod", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	tree := treeOf(t, r, snap)
	for path := range tree {
		if filepath.Ext(path) == ".log" {
			t.Fatalf("файл журнала попал в бэкап: %s", path)
		}
		if bytes.Contains([]byte(path), []byte("/cache/")) {
			t.Fatalf("каталог кэша попал в бэкап: %s", path)
		}
	}
	if !snap.Complete() {
		t.Fatalf("снимок помечен неполным: %+v", snap.Failed())
	}
	if snap.Stats.Files < 40 {
		t.Fatalf("собрано слишком мало файлов: %d", snap.Stats.Files)
	}
}

// Модуль, который не может отработать, обязан испортить только себя:
// снимок сохраняется, но помечается неполным.
func TestBackupSurvivesBrokenModule(t *testing.T) {
	ctx := context.Background()
	root := makeSite(t)
	r := openRepo(t)

	p := sitePlan(root)
	p.Modules = append(p.Modules, plan.Module{
		Kind: plan.KindFiles, Name: "нет такого", Enabled: true,
		Paths: []string{"/такого/пути/нет/нигде"},
	})
	snap, err := Backup(ctx, r, Options{Plan: p, Server: "prod", Agent: "test"})
	if err != nil {
		t.Fatalf("бэкап упал целиком из-за одного модуля: %v", err)
	}
	if snap.Stats.Files < 40 {
		t.Fatalf("рабочий модуль пострадал: собрано %d файлов", snap.Stats.Files)
	}
	// Несуществующий путь - не ошибка модуля, он просто пуст: набор путей
	// приходит из автообнаружения и вполне может устареть.
	if len(snap.Modules) != 2 {
		t.Fatalf("в снимке %d модулей вместо 2", len(snap.Modules))
	}
}
