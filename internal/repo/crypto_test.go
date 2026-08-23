package repo

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestChunkSealRoundtrip(t *testing.T) {
	k, err := NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("SELECT * FROM orders WHERE paid = 1")
	id := k.ChunkID(plain)

	ct, err := k.SealChunk(id, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("открытый текст виден в шифротексте")
	}
	got, err := k.OpenChunk(id, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("данные не совпали после расшифровки")
	}

	// Чанк, выданный под чужим id, не должен открываться: это защита от
	// подмены одного чанка другим внутри репозитория.
	other := k.ChunkID([]byte("что-то другое"))
	if _, err := k.OpenChunk(other, ct); err == nil {
		t.Fatal("чанк открылся под подменённым id")
	}
}

func TestChunkIDIsDeterministicAndKeyed(t *testing.T) {
	k1, _ := NewMasterKey()
	k2, _ := NewMasterKey()
	plain := []byte("одинаковое содержимое")

	if k1.ChunkID(plain) != k1.ChunkID(plain) {
		t.Fatal("id чанка не детерминирован - дедупликация работать не будет")
	}
	if k1.ChunkID(plain) == k2.ChunkID(plain) {
		t.Fatal("id чанка не зависит от ключа - репозиторий выдаёт своё содержимое")
	}
}

func TestMetaSealBoundToContext(t *testing.T) {
	k, _ := NewMasterKey()
	plain := []byte(`{"snapshot":"prod","files":42}`)

	ct, err := k.SealMeta("snapshots/aaaa", plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.OpenMeta("snapshots/bbbb", ct); err == nil {
		t.Fatal("снимок открылся под другим именем - перестановка файлов не заметна")
	}
	got, err := k.OpenMeta("snapshots/aaaa", ct)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("роундтрип метаданных сломан: %v", err)
	}
}

func TestPasswordWrapping(t *testing.T) {
	k, _ := NewMasterKey()
	// Параметры послабее: тест не должен молоть 64 МиБ памяти трижды.
	p := KDFParams{Time: 1, MemoryK: 8 * 1024, Threads: 1}

	kf, err := WrapMasterKey(k, "правильный пароль", "ноутбук", p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(kf.Sealed, k.raw) {
		t.Fatal("master key лежит в файле ключа открытым")
	}

	back, err := UnwrapMasterKey(kf, "правильный пароль")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.raw, k.raw) || back.ID() != k.ID() {
		t.Fatal("после распаковки получился другой ключ")
	}

	if _, err := UnwrapMasterKey(kf, "неправильный пароль"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("на неверный пароль ожидалась ErrWrongPassword, получено: %v", err)
	}
}

func TestRecoveryCode(t *testing.T) {
	k, _ := NewMasterKey()
	code := k.RecoveryCode()

	back, err := ParseRecoveryCode(code)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID() != k.ID() {
		t.Fatal("recovery-код восстановил не тот ключ")
	}

	// Код переписывают с бумаги руками: пробелы, переносы и регистр
	// не должны ничего ломать.
	messy := "  " + code[:12] + "\n" + code[12:] + "  "
	back2, err := ParseRecoveryCode(messy)
	if err != nil || back2.ID() != k.ID() {
		t.Fatalf("код не пережил переформатирование: %v", err)
	}
	t.Logf("recovery-код: %s", code)
}
