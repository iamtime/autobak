package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/repo"
)

// Учебное восстановление обязано подтверждать пригодность целого снимка.
func TestDrillConfirmsGoodSnapshot(t *testing.T) {
	ctx := context.Background()
	src, want := buildSite(t)
	r, snap := backupSite(t, src)

	rep, err := Drill(ctx, r, snap, DrillOptions{
		Log: func(l, m string) { t.Logf("[%s] %s", l, m) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())

	if !rep.OK() {
		t.Fatalf("целый снимок признан непригодным: %v %v", rep.Mismatches, rep.Problems)
	}
	if rep.FilesChecked != int64(len(want)) {
		t.Fatalf("проверено %d файлов из %d", rep.FilesChecked, len(want))
	}
}

// После проверки на диске не должно оставаться копии боевых данных.
func TestDrillCleansUpAfterItself(t *testing.T) {
	ctx := context.Background()
	src, _ := buildSite(t)
	r, snap := backupSite(t, src)

	before, _ := os.ReadDir(os.TempDir())
	if _, err := Drill(ctx, r, snap, DrillOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadDir(os.TempDir())

	leftover := 0
	seen := map[string]bool{}
	for _, e := range before {
		seen[e.Name()] = true
	}
	for _, e := range after {
		if !seen[e.Name()] && strings.HasPrefix(e.Name(), "autobak-drill-") {
			leftover++
		}
	}
	if leftover > 0 {
		t.Fatalf("после проверки осталось %d временных каталогов с данными", leftover)
	}
}

// Ограничение по объёму: на большом снимке проверяется выборка, а не всё.
func TestDrillRespectsByteBudget(t *testing.T) {
	ctx := context.Background()
	src, _ := buildSite(t)
	r, snap := backupSite(t, src)

	// Бюджет меньше самого крупного файла - должен взяться хотя бы один.
	rep, err := Drill(ctx, r, snap, DrillOptions{MaxBytes: 1000, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("выборочная проверка не прошла: %v %v", rep.Mismatches, rep.Problems)
	}
	if rep.FilesChecked == 0 {
		t.Fatal("при малом бюджете не проверено ни одного файла")
	}
	if rep.FilesChecked >= rep.FilesInSnap {
		t.Fatalf("бюджет не подействовал: проверено %d из %d", rep.FilesChecked, rep.FilesInSnap)
	}
	t.Logf("бюджет 1000 Б: %s", rep.Summary())
}

// Главное: испорченный репозиторий обязан быть признан непригодным
// именно учебным восстановлением, а не только проверкой подписей.
func TestDrillCatchesCorruptedRepo(t *testing.T) {
	ctx := context.Background()
	src, _ := buildSite(t)

	dir := t.TempDir()
	be, err := backend.OpenLocal(dir, backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := repo.Init(ctx, be, "пароль", "")
	if err != nil {
		t.Fatal(err)
	}
	snap := backupInto(t, r, src)

	// Портим пак: подпись перестанет сходиться, и склеить файл не выйдет.
	packID := r.Index().PackIDs()[0]
	p := filepath.Join(dir, filepath.FromSlash("data"), packID[:2], packID)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/3] ^= 0xFF
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := Drill(ctx, r, snap, DrillOptions{})
	// Восстановление может как упасть целиком, так и вернуть расхождения:
	// оба исхода означают «снимок непригоден», и ни один не должен
	// выглядеть как успех.
	if err == nil && rep.OK() {
		t.Fatal("испорченный репозиторий признан пригодным")
	}
	if rep != nil && !rep.OK() {
		t.Logf("обнаружено: %s", rep.Summary())
	} else {
		t.Logf("восстановление прервано: %v", err)
	}
}
