package repo

import (
	"bytes"
	"context"
	"testing"
)

// Оба пути записи обязаны давать одинаковые границы чанков. Иначе дерево,
// записанное push-путём, не дедуплицировалось бы с тем же содержимым,
// записанным pull-путём, и вся экономия рассыпалась бы незаметно.
func TestStreamWriterMatchesChunker(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRepo(t)
	data := realisticData(42, 24*MiB)

	w1, _ := r.NewWriter(ctx)
	viaReader, _, err := w1.WriteStream(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	w2, _ := r.NewWriter(ctx)
	sw := w2.NewStream()
	// Порции нарочно неровные: разбиение на Write не должно влиять
	// на границы чанков.
	for off := 0; off < len(data); {
		n := 7919
		if off+n > len(data) {
			n = len(data) - off
		}
		if _, err := sw.Write(data[off : off+n]); err != nil {
			t.Fatal(err)
		}
		off += n
	}
	viaPush, size, err := sw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	if size != int64(len(data)) {
		t.Fatalf("push-путь насчитал %d байт вместо %d", size, len(data))
	}
	if len(viaPush) != len(viaReader) {
		t.Fatalf("разное число чанков: push %d, pull %d", len(viaPush), len(viaReader))
	}
	for i := range viaPush {
		if viaPush[i] != viaReader[i] {
			t.Fatalf("чанк %d отличается между push- и pull-путями", i)
		}
	}
	t.Logf("оба пути дали одинаковые %d чанков", len(viaPush))
}
