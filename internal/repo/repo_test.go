package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

func testRepo(t *testing.T) (*Repo, backend.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := backend.OpenLocal(dir, backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r, recovery, err := Init(context.Background(), be, "пароль", "тест")
	if err != nil {
		t.Fatal(err)
	}
	if recovery == "" {
		t.Fatal("Init не вернул recovery-код")
	}
	return r, be, dir
}

func TestRepoRoundtrip(t *testing.T) {
	ctx := context.Background()
	r, be, dir := testRepo(t)

	payload := realisticData(7, 20*MiB)

	w, err := r.NewWriter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chunks, size, err := w.WriteStream(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("записано %d байт вместо %d", size, len(payload))
	}

	// Тот же поток второй раз обязан целиком уйти в дедупликацию.
	before := w.stats.ChunksNew
	same, _, err := w.WriteStream(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if w.stats.ChunksNew != before {
		t.Fatalf("повторная запись тех же данных создала %d новых чанков", w.stats.ChunksNew-before)
	}
	if len(same) != len(chunks) {
		t.Fatal("повторная запись дала другой список чанков")
	}

	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.BytesStored >= stats.BytesNew {
		t.Fatalf("сжатие не сработало: сохранено %d при исходных %d", stats.BytesStored, stats.BytesNew)
	}
	t.Logf("исходно %d МБ → в хранилище %d МБ (%d чанков)",
		stats.BytesTotal/MiB, stats.BytesStored/MiB, stats.ChunksNew)

	snap := &Snapshot{
		Time: time.Now(), Server: "prod", Hostname: "web-1", Agent: "test",
		Tree: chunks,
	}
	if err := r.SaveSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	be.Close()

	// Переоткрытие паролем: индексы должны подняться с нуля.
	be2, err := backend.OpenLocal(dir, backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Open(ctx, be2, "пароль")
	if err != nil {
		t.Fatal(err)
	}
	if r2.ID() != r.ID() {
		t.Fatal("после переоткрытия другой id репозитория")
	}
	if r2.Index().Count() != r.Index().Count() {
		t.Fatalf("индекс не восстановился: %d чанков вместо %d", r2.Index().Count(), r.Index().Count())
	}

	var got bytes.Buffer
	if _, err := r2.ReadStream(ctx, chunks, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatal("прочитанные данные не совпали с записанными")
	}

	snaps, err := r2.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Server != "prod" {
		t.Fatalf("список снимков неверен: %+v", snaps)
	}
}

func TestRepoWrongPassword(t *testing.T) {
	_, be, dir := testRepo(t)
	be.Close()

	be2, _ := backend.OpenLocal(dir, backend.Caps{CanWrite: true})
	if _, err := Open(context.Background(), be2, "не тот пароль"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("ожидалась ErrNoKey, получено: %v", err)
	}
}

// Инкрементальный сценарий: сайт, у которого поменялся один файл.
// Второй снимок обязан стоить единицы процентов от первого.
func TestRepoIncremental(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	day1 := realisticData(11, 40*MiB)
	w1, _ := r.NewWriter(ctx)
	if _, _, err := w1.WriteStream(bytes.NewReader(day1)); err != nil {
		t.Fatal(err)
	}
	s1, err := w1.Close()
	if err != nil {
		t.Fatal(err)
	}

	day2 := append([]byte(nil), day1[:15*MiB]...)
	day2 = append(day2, []byte("новая запись в логе заказов")...)
	day2 = append(day2, day1[15*MiB:]...)

	w2, _ := r.NewWriter(ctx)
	if _, _, err := w2.WriteStream(bytes.NewReader(day2)); err != nil {
		t.Fatal(err)
	}
	s2, err := w2.Close()
	if err != nil {
		t.Fatal(err)
	}

	ratio := float64(s2.BytesNew) / float64(s1.BytesNew)
	if ratio > 0.10 {
		t.Fatalf("второй снимок стоил %.1f%% от первого - дедупликация не работает", ratio*100)
	}
	t.Logf("день 1: %d МБ, день 2: %.2f МБ (%.1f%%)",
		s1.BytesNew/MiB, float64(s2.BytesNew)/MiB, ratio*100)
}

// Дерево файлов само лежит в репозитории как поток JSONL и должно
// читаться обратно узел за узлом.
func TestRepoTreeRoundtrip(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	nodes := []*Node{
		{Path: "/home/admin/web/site.ru", Type: NodeDir, Mode: 0o755, User: "admin", Group: "admin"},
		{Path: "/home/admin/web/site.ru/index.php", Type: NodeFile, Mode: 0o644, Size: 12, User: "admin"},
		{Path: "/home/admin/web/site.ru/current", Type: NodeSymlink, Link: "releases/42"},
	}

	w, _ := r.NewWriter(ctx)
	pr, pw := io.Pipe()
	go func() {
		enc := json.NewEncoder(pw)
		for _, n := range nodes {
			if err := enc.Encode(n); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	tree, _, err := w.WriteStream(pr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	snap := &Snapshot{Time: time.Now(), Server: "prod", Tree: tree}
	if err := r.SaveSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	var got []*Node
	if err := r.ReadTree(ctx, snap, func(n *Node) error {
		got = append(got, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(nodes) {
		t.Fatalf("прочитано %d узлов вместо %d", len(got), len(nodes))
	}
	if got[2].Link != "releases/42" || got[1].Mode != 0o644 {
		t.Fatalf("узлы прочитались с потерями: %+v", got)
	}
}

// Повреждённый пак обязан выявляться при чтении, а не отдавать мусор.
func TestRepoDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	r, be, _ := testRepo(t)

	w, _ := r.NewWriter(ctx)
	ids, _, err := w.WriteStream(bytes.NewReader(realisticData(13, 3*MiB)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	packID := r.Index().PackIDs()[0]
	name := packName(packID)
	raw, err := backend.ReadAll(ctx, be, name, 64*MiB)
	if err != nil {
		t.Fatal(err)
	}
	raw[100] ^= 0xFF // один перевёрнутый бит в теле пака
	// Кэш пака мог остаться с прошлого чтения - он не должен спасать подделку.
	r.cacheID, r.cacheB = "", nil
	if err := be.Put(ctx, name, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}

	var sink bytes.Buffer
	if _, err := r.ReadStream(ctx, ids, &sink); err == nil {
		t.Fatal("повреждение пака прошло незамеченным")
	}
}
