package secretstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.dat")
	s := New(path)

	if _, err := s.Get("нет"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("на пустом хранилище ожидалась ErrNotFound, получено %v", err)
	}
	secret := "s3-ключ-очень-секретный-2026"
	if err := s.Set("repo/prod/s3", secret); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("секрет лежит на диске открытым текстом")
	}

	// Новый экземпляр обязан прочитать то же самое.
	s2 := New(path)
	got, err := s2.Get("repo/prod/s3")
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("прочитано %q вместо %q", got, secret)
	}
	if !s2.Has("repo/prod/s3") {
		t.Fatal("Has не видит существующий секрет")
	}

	if err := s2.Delete("repo/prod/s3"); err != nil {
		t.Fatal(err)
	}
	if New(path).Has("repo/prod/s3") {
		t.Fatal("удалённый секрет всё ещё доступен")
	}
}
