package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func snapAt(id string, server string, t time.Time) *Snapshot {
	return &Snapshot{ID: id, Server: server, Time: t}
}

func TestRetentionGFS(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.Local)
	var snaps []*Snapshot
	// Год ежедневных снимков.
	for i := range 365 {
		d := now.AddDate(0, 0, -i)
		snaps = append(snaps, snapAt(d.Format("20060102"), "prod", d))
	}

	p := Retention{Last: 3, Daily: 7, Weekly: 4, Monthly: 6, Yearly: 1}
	keep, remove := p.Apply(snaps, now)

	if len(keep)+len(remove) != len(snaps) {
		t.Fatal("снимки потерялись при делении")
	}
	// 7 дневных + 4 недельных + 6 месячных + 1 годовой, с перекрытиями.
	if len(keep) < 12 || len(keep) > 20 {
		t.Fatalf("оставлено %d снимков - политика посчитана неверно", len(keep))
	}
	// Свежайший обязан остаться при любых обстоятельствах.
	if keep[0].ID != now.Format("20060102") {
		t.Fatalf("последний снимок не сохранён, первый в списке: %s", keep[0].ID)
	}
	t.Logf("из 365 снимков осталось %d", len(keep))
}

func TestRetentionKeepsEachServerSeparately(t *testing.T) {
	now := time.Now()
	var snaps []*Snapshot
	for i := range 20 {
		snaps = append(snaps, snapAt("a"+string(rune('a'+i)), "prod", now.AddDate(0, 0, -i)))
	}
	// Второй сервер бэкапится редко - его историю нельзя вымывать
	// счётчиками, заполненными снимками первого.
	snaps = append(snaps, snapAt("old1", "dev", now.AddDate(0, 0, -200)))
	snaps = append(snaps, snapAt("old2", "dev", now.AddDate(0, 0, -400)))

	keep, _ := Retention{Last: 2, Daily: 3}.Apply(snaps, now)
	devKept := 0
	for _, s := range keep {
		if s.Server == "dev" {
			devKept++
		}
	}
	if devKept != 2 {
		t.Fatalf("у сервера dev осталось %d снимков вместо 2", devKept)
	}
}

func TestRetentionNeverEmpties(t *testing.T) {
	now := time.Now()
	old := []*Snapshot{snapAt("ancient", "prod", now.AddDate(-5, 0, 0))}
	keep, remove := Retention{Last: 0, Daily: 1}.Apply(old, now)
	if len(keep) != 1 || len(remove) != 0 {
		t.Fatal("единственный снимок сервера был удалён политикой хранения")
	}
}

// writeSnapshot кладёт payload в репозиторий как один файл и оформляет
// вокруг него полноценный снимок с деревом.
func writeSnapshot(t *testing.T, r *Repo, id, server string, when time.Time, payload []byte) []ChunkID {
	t.Helper()
	ctx := context.Background()
	w, err := r.NewWriter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chunks, size, err := w.WriteStream(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{Path: "/srv/data.bin", Type: NodeFile, Mode: 0o644, Size: size, Chunks: chunks}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(json.NewEncoder(pw).Encode(node))
	}()
	tree, _, err := w.WriteStream(pr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Snapshot{ID: id, Server: server, Time: when, Tree: tree,
		Stats: SnapshotStats{Files: 1, BytesTotal: size}}
	if err := r.SaveSnapshot(ctx, s); err != nil {
		t.Fatal(err)
	}
	return chunks
}

func TestPruneFreesSpaceAndKeepsData(t *testing.T) {
	ctx := context.Background()
	r, be, dir := testRepo(t)

	// Три снимка с независимыми данными: старые должны уйти целиком.
	var keepChunks []ChunkID
	for i := range 3 {
		keepChunks = writeSnapshot(t, r, "snap"+string(rune('1'+i)), "prod",
			time.Now().AddDate(0, 0, -(2-i)), realisticData(int64(100+i), 12*MiB))
	}

	sizeBefore := dirSize(t, dir)

	// Сухой прогон обязан ничего не менять.
	opt := DefaultPruneOptions()
	opt.Policy = Retention{Last: 1}
	opt.DryRun = true
	dry, err := r.Prune(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.SnapshotsRemoved) != 2 {
		t.Fatalf("dry-run насчитал %d снимков к удалению вместо 2", len(dry.SnapshotsRemoved))
	}
	if dirSize(t, dir) != sizeBefore {
		t.Fatal("dry-run изменил репозиторий")
	}

	opt.DryRun = false
	rep, err := r.Prune(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())

	sizeAfter := dirSize(t, dir)
	if sizeAfter >= sizeBefore {
		t.Fatalf("prune не освободил места: было %s, стало %s",
			HumanBytes(sizeBefore), HumanBytes(sizeAfter))
	}

	// Главное: оставшийся снимок обязан по-прежнему читаться.
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("после prune осталось %d снимков вместо 1", len(snaps))
	}
	var got bytes.Buffer
	if _, err := r.ReadStream(ctx, keepChunks, &got); err != nil {
		t.Fatalf("данные оставленного снимка не читаются после prune: %v", err)
	}
	if got.Len() != 12*MiB {
		t.Fatalf("прочитано %d байт вместо %d", got.Len(), 12*MiB)
	}
	be.Close()
}

func TestVerifyDetectsMissingAndBroken(t *testing.T) {
	ctx := context.Background()
	r, be, dir := testRepo(t)

	writeSnapshot(t, r, "s1", "prod", time.Now(), realisticData(200, 8*MiB))

	rep, err := r.Verify(ctx, VerifyOptions{Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("целый репозиторий признан битым: %v", rep.Problems)
	}
	t.Log(rep.Summary())

	// Портим пак и проверяем, что это замечено.
	packID := r.Index().PackIDs()[0]
	p := filepath.Join(dir, filepath.FromSlash(packName(packID)))
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0x01
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r.cacheID, r.cacheB = "", nil

	rep2, err := r.Verify(ctx, VerifyOptions{Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.OK() {
		t.Fatal("повреждение не обнаружено полной проверкой")
	}
	t.Log(rep2.Summary())
	be.Close()
}

// Потеря каталога index/ не должна означать потерю данных: каждый пак
// самоописывающийся, и индекс собирается заново по их хвостам.
func TestRebuildIndexAfterLosingIt(t *testing.T) {
	ctx := context.Background()
	r, _, dir := testRepo(t)

	w, _ := r.NewWriter(ctx)
	payload := realisticData(300, 30*MiB)
	ids, _, err := w.WriteStream(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before := r.Index().Count()

	if err := os.RemoveAll(filepath.Join(dir, DirIndex)); err != nil {
		t.Fatal(err)
	}
	n, err := r.RebuildIndex(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Index().Count() != before {
		t.Fatalf("после пересборки %d чанков вместо %d (паков %d)", r.Index().Count(), before, n)
	}

	var got bytes.Buffer
	if _, err := r.ReadStream(ctx, ids, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatal("после пересборки индекса данные читаются неверно")
	}
	t.Logf("индекс собран заново по %d пакам, %d чанков", n, r.Index().Count())
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// Очистка одного сервера в общем репозитории не должна трогать историю
// другого. Регрессия на находку аудита: раньше Prune применял политику
// запустившего сервера ко всем снимкам подряд и выкашивал чужие.
func TestPruneServerScopeKeepsOtherServers(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)

	// Сервер A: три снимка. Сервер B: три снимка «которые хранятся всегда».
	for i := range 3 {
		writeSnapshot(t, r, "a"+string(rune('1'+i)), "serverA",
			time.Now().AddDate(0, 0, -(2-i)), realisticData(int64(300+i), 8*MiB))
	}
	var bChunks [][]ChunkID
	for i := range 3 {
		bChunks = append(bChunks, writeSnapshot(t, r, "b"+string(rune('1'+i)), "serverB",
			time.Now().AddDate(0, 0, -(2-i)), realisticData(int64(400+i), 8*MiB)))
	}

	// Очистка сервера A с политикой «1 последний».
	opt := DefaultPruneOptions()
	opt.Policy = Retention{Last: 1}
	opt.Server = "serverA"
	rep, err := r.Prune(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}

	// Удалены должны быть только два старых снимка A.
	if len(rep.SnapshotsRemoved) != 2 {
		t.Fatalf("удалено %d снимков, ожидалось 2 (только сервера A): %v",
			len(rep.SnapshotsRemoved), rep.SnapshotsRemoved)
	}
	for _, id := range rep.SnapshotsRemoved {
		if id[0] != 'a' {
			t.Fatalf("prune удалил снимок чужого сервера: %s", id)
		}
	}

	// Все три снимка B на месте и читаются.
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bLeft := 0
	for _, s := range snaps {
		if s.Server == "serverB" {
			bLeft++
		}
	}
	if bLeft != 3 {
		t.Fatalf("у сервера B осталось %d снимков вместо 3", bLeft)
	}

	// И данные B физически целы: чанки каждого снимка читаются.
	for i, chunks := range bChunks {
		for _, id := range chunks {
			if _, err := r.LoadChunk(ctx, id); err != nil {
				t.Fatalf("чанк снимка B%d потерян после очистки A: %v", i+1, err)
			}
		}
	}
}

// Verify обязан замечать нечитаемый манифест снимка, а не рапортовать
// «всё цело». Регрессия на находку аудита: раньше ListSnapshots молча
// пропускал битый снимок, и проверка его не видела.
func TestVerifyDetectsUnreadableManifest(t *testing.T) {
	ctx := context.Background()
	r, be, dir := testRepo(t)
	writeSnapshot(t, r, "s1", "prod", time.Now(), realisticData(210, 8*MiB))

	// Портим файл манифеста.
	p := filepath.Join(dir, filepath.FromSlash(DirSnapshots+"/s1"))
	if err := os.WriteFile(p, []byte("мусор, не расшифруется"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := r.Verify(ctx, VerifyOptions{Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("verify не заметил нечитаемый манифест снимка")
	}

	// А запрос проверки несуществующего снимка не должен рапортовать успех.
	rep2, err := r.Verify(ctx, VerifyOptions{Sample: 1, Snapshots: []string{"нет-такого"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.OK() {
		t.Fatal("verify несуществующего снимка отрапортовал успех")
	}
	be.Close()
}

// Prune обязан остановиться, а не удалить данные, если снимок не читается.
func TestPruneStopsOnUnreadableManifest(t *testing.T) {
	ctx := context.Background()
	r, be, dir := testRepo(t)
	defer be.Close()
	writeSnapshot(t, r, "keep", "prod", time.Now(), realisticData(220, 8*MiB))
	writeSnapshot(t, r, "old", "prod", time.Now().AddDate(0, 0, -10), realisticData(221, 8*MiB))

	// Ломаем один манифест.
	p := filepath.Join(dir, filepath.FromSlash(DirSnapshots+"/old"))
	if err := os.WriteFile(p, []byte("битый"), 0o600); err != nil {
		t.Fatal(err)
	}

	opt := DefaultPruneOptions()
	opt.Policy = Retention{Last: 1}
	if _, err := r.Prune(ctx, opt); err == nil {
		t.Fatal("prune не остановился на нечитаемом снимке - риск удалить его чанки")
	}
}
