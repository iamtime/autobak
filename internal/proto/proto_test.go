package proto

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/repo"
)

func TestFrameRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.JSON(FrameHello, Hello{Version: Version, Agent: "test", Hostname: "web-1"}); err != nil {
		t.Fatal(err)
	}
	node := &repo.Node{Path: "/etc/nginx/nginx.conf", Type: repo.NodeFile, Mode: 0o644, Size: 5}
	if err := w.Node(node); err != nil {
		t.Fatal(err)
	}
	if err := w.Data([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.NodeEnd(); err != nil {
		t.Fatal(err)
	}
	if err := w.JSON(FrameDone, Done{Files: 1, Bytes: 5}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	r := NewReader(&buf)
	want := []FrameType{FrameHello, FrameNode, FrameData, FrameNodeEnd, FrameDone}
	for i, expect := range want {
		ft, payload, err := r.Next()
		if err != nil {
			t.Fatalf("кадр %d: %v", i, err)
		}
		if ft != expect {
			t.Fatalf("кадр %d: получен %s, ожидался %s", i, ft, expect)
		}
		switch ft {
		case FrameNode:
			n, err := DecodeJSON[repo.Node](payload)
			if err != nil || n.Path != node.Path || n.Mode != node.Mode {
				t.Fatalf("узел приехал искажённым: %+v (%v)", n, err)
			}
		case FrameData:
			if string(payload) != "hello" {
				t.Fatalf("данные приехали как %q", payload)
			}
		}
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("после последнего кадра ожидался EOF, получено %v", err)
	}
}

// Длина кадра приходит из потока, то есть от другой стороны. Абсурдное
// значение не должно превращаться в попытку выделить гигабайты памяти.
func TestReaderRejectsHugeFrame(t *testing.T) {
	// Тип 3 (данные) и длина 4 ГБ.
	bad := []byte{3, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, _, err := NewReader(bytes.NewReader(bad)).Next(); err == nil {
		t.Fatal("кадр абсурдного размера был принят")
	}
}

func TestReaderDetectsTruncation(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Data(bytes.Repeat([]byte("x"), 100))
	w.Flush()

	// Обрезаем поток посередине - так выглядит оборванное SSH-соединение.
	cut := buf.Bytes()[:len(buf.Bytes())-40]
	if _, _, err := NewReader(bytes.NewReader(cut)).Next(); err == nil {
		t.Fatal("обрезанный кадр прочитался без ошибки")
	}
}

func TestCopyStreamSplitsLargeFiles(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Больше одного кадра: содержимое обязано резаться на части,
	// а не собираться в памяти целиком.
	size := MaxDataFrame*2 + 12345
	src := strings.NewReader(strings.Repeat("a", size))

	var counted int
	n, err := w.CopyStream(src, nil, func(k int) { counted += k })
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(size) || counted != size {
		t.Fatalf("передано %d байт, счётчик %d, ожидалось %d", n, counted, size)
	}
	w.Flush()

	r := NewReader(&buf)
	var frames, total int
	for {
		ft, payload, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ft != FrameData {
			t.Fatalf("неожиданный кадр %s", ft)
		}
		frames++
		total += len(payload)
	}
	if total != size {
		t.Fatalf("собрано %d байт вместо %d", total, size)
	}
	if frames < 3 {
		t.Fatalf("поток разбит всего на %d кадров - буферизуется целиком", frames)
	}
}

func TestFrameTypeNames(t *testing.T) {
	if FrameNode.String() != "node" || FrameError.String() != "error" {
		t.Fatal("имена кадров не совпадают с ожидаемыми")
	}
	if !strings.Contains(FrameType(99).String(), "99") {
		t.Fatal("неизвестный кадр должен печататься с номером")
	}
}
