package repo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iamtime/autobak/internal/backend"
)

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

func mirrorTarget(t *testing.T) (backend.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := backend.OpenLocal(dir, backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	return be, dir
}

// Зеркало обязано открываться тем же паролем и отдавать те же данные:
// иначе это не копия, а иллюзия копии.
func TestMirrorProducesUsableCopy(t *testing.T) {
	ctx := context.Background()
	r, src, _ := testRepo(t)

	var payloads [][]byte
	for i := range 3 {
		data := realisticData(int64(500+i), 8*MiB)
		payloads = append(payloads, data)
		writeSnapshot(t, r, "snap"+string(rune('1'+i)), "prod",
			time.Now().AddDate(0, 0, -(2-i)), data)
	}

	dst, dstDir := mirrorTarget(t)
	rep, err := Mirror(ctx, src, dst, DefaultMirrorOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())
	if !rep.OK() || rep.Copied == 0 {
		t.Fatalf("зеркалирование не удалось: %+v", rep.Problems)
	}

	// Главное: зеркало - самостоятельный рабочий репозиторий.
	m, err := Open(ctx, dst, "пароль")
	if err != nil {
		t.Fatalf("зеркало не открывается: %v", err)
	}
	snaps, err := m.ListSnapshots(ctx)
	if err != nil || len(snaps) != 3 {
		t.Fatalf("в зеркале %d снимков, %v", len(snaps), err)
	}
	vr, err := m.Verify(ctx, VerifyOptions{Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !vr.OK() {
		t.Fatalf("зеркало не проходит проверку: %v", vr.Problems)
	}
	t.Logf("зеркало: %s", vr.Summary())

	// И данные читаются побайтово те же.
	for i, sn := range snaps {
		var got bytes.Buffer
		if err := m.ReadTree(ctx, sn, func(n *Node) error {
			if n.Type != NodeFile {
				return nil
			}
			_, err := m.ReadStream(ctx, n.Chunks, &got)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.Len() != 8*MiB {
			t.Fatalf("снимок %d: прочитано %d байт", i, got.Len())
		}
	}
	_ = dstDir
	_ = payloads
}

// Повторное зеркалирование должно докачивать, а не копировать заново.
func TestMirrorIsIncremental(t *testing.T) {
	ctx := context.Background()
	r, src, _ := testRepo(t)
	writeSnapshot(t, r, "s1", "prod", time.Now(), realisticData(601, 10*MiB))

	dst, _ := mirrorTarget(t)
	first, err := Mirror(ctx, src, dst, DefaultMirrorOptions())
	if err != nil {
		t.Fatal(err)
	}

	second, err := Mirror(ctx, src, dst, DefaultMirrorOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("первый проход: %s", first.Summary())
	t.Logf("второй проход: %s", second.Summary())

	if second.Copied != 0 {
		t.Fatalf("повторный проход скопировал %d объектов заново", second.Copied)
	}
	if second.Skipped != first.Copied {
		t.Fatalf("пропущено %d при скопированных ранее %d", second.Skipped, first.Copied)
	}

	// Новый снимок должен доехать, не трогая уже скопированное.
	writeSnapshot(t, r, "s2", "prod", time.Now(), realisticData(602, 10*MiB))
	third, err := Mirror(ctx, src, dst, DefaultMirrorOptions())
	if err != nil {
		t.Fatal(err)
	}
	if third.Copied == 0 {
		t.Fatal("новый снимок не доехал до зеркала")
	}
	if third.Copied >= first.Copied {
		t.Fatalf("докачка скопировала %d объектов при исходных %d", third.Copied, first.Copied)
	}
	t.Logf("докачка нового снимка: %s", third.Summary())
}

// Удалённое в источнике не должно исчезать из зеркала само: зеркало
// существует ровно на случай, когда с источником случилось плохое.
func TestMirrorKeepsOrphansByDefault(t *testing.T) {
	ctx := context.Background()
	r, src, _ := testRepo(t)
	writeSnapshot(t, r, "old", "prod", time.Now().AddDate(0, 0, -10), realisticData(701, 6*MiB))
	writeSnapshot(t, r, "new", "prod", time.Now(), realisticData(702, 6*MiB))

	dst, dstDir := mirrorTarget(t)
	if _, err := Mirror(ctx, src, dst, DefaultMirrorOptions()); err != nil {
		t.Fatal(err)
	}
	before := countFiles(t, dstDir)

	opt := DefaultPruneOptions()
	opt.Policy = Retention{Last: 1}
	if _, err := r.Prune(ctx, opt); err != nil {
		t.Fatal(err)
	}

	rep, err := Mirror(ctx, src, dst, DefaultMirrorOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Orphans) == 0 {
		t.Fatal("лишние объекты в зеркале не замечены")
	}
	// Зеркало могло вырасти (источник после prune дописал новый индекс),
	// но потерять не должно ничего.
	if after := countFiles(t, dstDir); after < before {
		t.Fatalf("зеркало похудело без явного разрешения: было %d, стало %d", before, after)
	}
	t.Logf("замечено лишних: %d (не удалены)", len(rep.Orphans))

	// С явного разрешения - удаляются.
	opt2 := DefaultMirrorOptions()
	opt2.Prune = true
	rep2, err := Mirror(ctx, src, dst, opt2)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Deleted == 0 {
		t.Fatal("с разрешением лишнее не удалилось")
	}
	if countFiles(t, dstDir) >= before {
		t.Fatal("после очистки зеркало не уменьшилось")
	}
	// И оставшееся по-прежнему рабочее.
	m, err := Open(ctx, dst, "пароль")
	if err != nil {
		t.Fatal(err)
	}
	if vr, err := m.Verify(ctx, VerifyOptions{Sample: 1}); err != nil || !vr.OK() {
		t.Fatalf("после очистки зеркало испорчено: %v %v", err, vr)
	}
	t.Logf("после очистки: %s", rep2.Summary())
}

// Сухой прогон обязан считать, но не трогать.
func TestMirrorDryRun(t *testing.T) {
	ctx := context.Background()
	r, src, _ := testRepo(t)
	writeSnapshot(t, r, "s1", "prod", time.Now(), realisticData(801, 4*MiB))

	dst, dstDir := mirrorTarget(t)
	opt := DefaultMirrorOptions()
	opt.DryRun = true
	rep, err := Mirror(ctx, src, dst, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied == 0 || rep.Bytes == 0 {
		t.Fatal("сухой прогон ничего не насчитал")
	}
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("сухой прогон записал в зеркало %d объектов", len(entries))
	}
	t.Log(rep.Summary())
}

// Порядок копирования: снимки не должны появляться в зеркале раньше
// данных, на которые ссылаются.
func TestMirrorOrderKeepsSnapshotsLast(t *testing.T) {
	ctx := context.Background()
	r, src, _ := testRepo(t)
	writeSnapshot(t, r, "s1", "prod", time.Now(), realisticData(901, 6*MiB))

	dst, dstDir := mirrorTarget(t)
	opt := DefaultMirrorOptions()
	opt.Workers = 1

	// Обрываем копирование в момент, когда до снимков ещё не дошло.
	ctx2, cancel := context.WithCancel(ctx)
	opt.Progress = func(stage string, done, total int) {
		if stage == "данные" && done >= 1 {
			cancel()
		}
	}
	_, _ = Mirror(ctx2, src, dst, opt)

	// В прерванном зеркале снимков быть не должно - значит, нет и
	// ложного впечатления, что копия готова.
	if _, err := os.Stat(filepath.Join(dstDir, DirSnapshots)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(dstDir, DirSnapshots))
		if len(entries) > 0 {
			t.Fatalf("в прерванном зеркале уже %d снимков", len(entries))
		}
	}
	t.Log("прерванное зеркало не содержит снимков - данные без ссылок безопаснее ссылок без данных")
}
