package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
)

// countingWriter считает, сколько байт реально прошло по каналу.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// runPair сводит агента и десктоп через две трубы, как это делает SSH:
// вывод агента читает десктоп, вывод десктопа читает агент.
//
// Возвращает снимок и число байт, ушедших от агента к десктопу, - то есть
// ровно то, за что платит человек с медленным каналом.
func runPair(t *testing.T, r *repo.Repo, p *plan.Plan,
	known map[repo.ChunkID]struct{}, wireDedup bool) (*repo.Snapshot, int64) {
	t.Helper()
	ctx := context.Background()

	planJSON, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	req := proto.Request{Plan: planJSON}
	if wireDedup {
		req.ChunkKey = hex.EncodeToString(r.Key().ChunkIDKey())
		req.Chunker = r.Chunker()
	}

	// io.Pipe отдаёт пару «читатель, писатель»: первая труба несёт данные
	// от агента к десктопу, вторая - ответы обратно.
	desktopIn, agentOut := io.Pipe()
	agentIn, desktopOut := io.Pipe()
	counted := &countingWriter{w: agentOut}

	errc := make(chan error, 1)
	go func() {
		err := Export(ctx, counted, agentIn, &req, "test")
		agentOut.CloseWithError(err)
		errc <- err
	}()

	snap, err := Import(ctx, r, desktopIn, desktopOut, Options{
		Server: "prod", Known: known,
		Log: func(l, m string) {
			if l != "info" {
				t.Logf("[%s] %s", l, m)
			}
		},
	})
	desktopOut.Close()
	if exportErr := <-errc; exportErr != nil {
		t.Fatalf("агент: %v", exportErr)
	}
	if err != nil {
		t.Fatalf("десктоп: %v", err)
	}
	return snap, counted.n
}

func chunksOf(t *testing.T, r *repo.Repo, snap *repo.Snapshot) map[repo.ChunkID]struct{} {
	t.Helper()
	out := map[repo.ChunkID]struct{}{}
	for _, id := range snap.Tree {
		out[id] = struct{}{}
	}
	if err := r.ReadTree(context.Background(), snap, func(n *repo.Node) error {
		for _, id := range n.Chunks {
			out[id] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Главное свойство: повторный бэкап не должен гнать по сети то, что уже
// лежит в хранилище. Без этого инкрементальность экономит место, но не
// канал, и сайт на 4 ГБ качается заново каждую ночь.
func TestWireDedupSendsOnlyNewData(t *testing.T) {
	root := makeSite(t)
	p := sitePlan(root)
	r := openRepo(t)

	first, wire1 := runPair(t, r, p, nil, true)
	t.Logf("первый бэкап: данных %s, по сети %s",
		repo.HumanBytes(first.Stats.BytesTotal), repo.HumanBytes(wire1))

	known := chunksOf(t, r, first)
	second, wire2 := runPair(t, r, p, known, true)
	t.Logf("повторный без изменений: по сети %s (%.2f%% от первого)",
		repo.HumanBytes(wire2), float64(wire2)/float64(wire1)*100)

	if second.Stats.BytesNew != 0 {
		t.Errorf("в хранилище записано %s при отсутствии изменений",
			repo.HumanBytes(second.Stats.BytesNew))
	}
	if wire2 > wire1/20 {
		t.Fatalf("по сети ушло %s из %s - дедупликация на стороне сервера не работает",
			repo.HumanBytes(wire2), repo.HumanBytes(wire1))
	}

	// Правка одного файла обязана стоить примерно этот файл, а не весь сайт.
	victim := filepath.Join(root, "public_html", "index.php")
	if err := os.WriteFile(victim, []byte(strings.Repeat("изменено ", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	known = chunksOf(t, r, second)
	third, wire3 := runPair(t, r, p, known, true)
	t.Logf("после правки одного файла: по сети %s", repo.HumanBytes(wire3))

	if wire3 > wire1/10 {
		t.Fatalf("правка одного файла стоила %s трафика при полном объёме %s",
			repo.HumanBytes(wire3), repo.HumanBytes(wire1))
	}
	if third.Stats.Files != first.Stats.Files {
		t.Fatalf("после правки в снимке %d файлов вместо %d",
			third.Stats.Files, first.Stats.Files)
	}
}

// Дедупликация по сети не должна менять содержимое снимка: деревья
// с ней и без неё обязаны совпадать до чанка.
func TestWireDedupProducesSameSnapshot(t *testing.T) {
	root := makeSite(t)
	p := sitePlan(root)
	r := openRepo(t)

	plainSnap, wirePlain := runPair(t, r, p, nil, false)
	dedupSnap, wireDedup := runPair(t, r, p, nil, true)
	t.Logf("без дедупликации по сети %s, с ней %s",
		repo.HumanBytes(wirePlain), repo.HumanBytes(wireDedup))

	a := treeOf(t, r, plainSnap)
	b := treeOf(t, r, dedupSnap)
	if len(a) != len(b) {
		t.Fatalf("узлов: %d против %d", len(a), len(b))
	}
	for path, na := range a {
		nb, ok := b[path]
		if !ok {
			t.Fatalf("узел %s пропал", path)
		}
		if na.Size != nb.Size || len(na.Chunks) != len(nb.Chunks) {
			t.Fatalf("узел %s отличается: %d/%d байт, %d/%d чанков",
				path, na.Size, nb.Size, len(na.Chunks), len(nb.Chunks))
		}
		for i := range na.Chunks {
			if na.Chunks[i] != nb.Chunks[i] {
				t.Fatalf("узел %s: чанк %d отличается", path, i)
			}
		}
	}
}

// Агент называет чанки сам. Взломанный сервер может назвать своим чужой
// идентификатор - и тогда в общем репозитории подменится содержимое.
// Десктоп обязан пересчитывать идентификатор, а не верить на слово.
func TestImportRejectsForgedChunk(t *testing.T) {
	ctx := context.Background()
	r := openRepo(t)

	var stream strings.Builder
	w := proto.NewWriter(&stream)
	if err := w.JSON(proto.FrameHello, proto.Hello{Version: proto.Version, Agent: "злой"}); err != nil {
		t.Fatal(err)
	}
	honest := []byte("настоящее содержимое")
	realID := r.Key().ChunkID(honest)
	if err := w.Node(&repo.Node{
		Path: "/tmp/x", Type: repo.NodeFile, Size: int64(len(honest)),
		Chunks: []repo.ChunkID{realID},
	}); err != nil {
		t.Fatal(err)
	}
	// Содержимое подменено, идентификатор оставлен настоящий.
	if err := w.ChunkRaw(realID, []byte("подложенное содержимое")); err != nil {
		t.Fatal(err)
	}
	if err := w.JSON(proto.FrameDone, proto.Done{Files: 1}); err != nil {
		t.Fatal(err)
	}
	w.Flush()

	_, err := Import(ctx, r, strings.NewReader(stream.String()), io.Discard, Options{Server: "prod"})
	if err == nil {
		t.Fatal("подложенный чанк был принят")
	}
	if !strings.Contains(err.Error(), "не соответствует") {
		t.Fatalf("ошибка не про подмену: %v", err)
	}
	t.Logf("отклонено: %v", err)
}

// Ссылка на чанк, который так и не приехал, обязана останавливать бэкап:
// иначе снимок сохранился бы с дырой, и обнаружилось бы это только при
// попытке восстановиться.
func TestImportRejectsMissingChunk(t *testing.T) {
	ctx := context.Background()
	r := openRepo(t)

	var stream strings.Builder
	w := proto.NewWriter(&stream)
	w.JSON(proto.FrameHello, proto.Hello{Version: proto.Version, Agent: "забывчивый"})
	w.Node(&repo.Node{
		Path: "/tmp/x", Type: repo.NodeFile, Size: 10,
		Chunks: []repo.ChunkID{r.Key().ChunkID([]byte("никогда не приедет"))},
	})
	w.JSON(proto.FrameDone, proto.Done{Files: 1})
	w.Flush()

	_, err := Import(ctx, r, strings.NewReader(stream.String()), io.Discard, Options{Server: "prod"})
	if err == nil {
		t.Fatal("снимок с недостающим чанком был сохранён")
	}
	if !strings.Contains(err.Error(), "не передал") {
		t.Fatalf("ошибка не про пропущенный чанк: %v", err)
	}
	t.Logf("отклонено: %v", err)
}
