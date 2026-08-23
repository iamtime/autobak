package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
)

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// --- Сторона агента: сборщики → поток кадров ------------------------------

// chunkBatch накапливает нарезанные чанки и выясняет у десктопа, какие
// из них там уже есть.
//
// Спрашивать про каждый чанк отдельно нельзя: на канале с пингом 30 мс
// сайт из десяти тысяч файлов означал бы час чистого ожидания. Поэтому
// вопрос задаётся пачками, а размер пачки ограничен и по числу чанков,
// и по объёму - чтобы память агента не зависела от размера бэкапа.
type chunkBatch struct {
	w  *proto.Writer
	in *proto.Reader

	ids  []repo.ChunkID
	data [][]byte
	held int

	// Wire - сколько байт содержимого реально ушло по сети,
	// Saved - сколько удалось не отправлять.
	Wire, Saved int64
}

const (
	batchMaxChunks = 512
	batchMaxBytes  = 8 << 20
)

func (b *chunkBatch) add(id repo.ChunkID, chunk []byte) error {
	// Копия обязательна: чанкер отдаёт срез своего буфера, который
	// перезапишется на следующем шаге.
	b.ids = append(b.ids, id)
	b.data = append(b.data, append([]byte(nil), chunk...))
	b.held += len(chunk)
	if len(b.ids) >= batchMaxChunks || b.held >= batchMaxBytes {
		return b.flush()
	}
	return nil
}

func (b *chunkBatch) flush() error {
	if len(b.ids) == 0 {
		return nil
	}
	if err := b.w.JSON(proto.FrameNeed, b.ids); err != nil {
		return err
	}
	// Без сброса буфера вопрос не уйдёт, и обе стороны будут ждать друг друга.
	if err := b.w.Flush(); err != nil {
		return err
	}

	t, payload, err := b.in.Next()
	if err != nil {
		return fmt.Errorf("autobak: десктоп не ответил на запрос о чанках: %w", err)
	}
	if t != proto.FrameHave {
		return fmt.Errorf("autobak: ожидался ответ о чанках, получен кадр %s", t)
	}
	have, err := proto.DecodeJSON[proto.Have](payload)
	if err != nil {
		return err
	}
	if len(have.Known) != len(b.ids) {
		return fmt.Errorf("autobak: ответ о чанках не той длины: %d вместо %d",
			len(have.Known), len(b.ids))
	}

	for i, id := range b.ids {
		if have.Known[i] {
			b.Saved += int64(len(b.data[i]))
			continue
		}
		if err := b.w.ChunkRaw(id, b.data[i]); err != nil {
			return err
		}
		b.Wire += int64(len(b.data[i]))
	}
	b.ids, b.data, b.held = b.ids[:0], b.data[:0], 0
	return nil
}

type protoSink struct {
	w     *proto.Writer
	buf   []byte
	files int64
	dirs  int64
	bytes int64

	// Заполнены, только когда десктоп попросил не гонять известное.
	hasher *repo.ChunkHasher
	chp    repo.ChunkerParams
	batch  *chunkBatch

	stage      string
	lastReport time.Time
}

func (s *protoSink) dedup() bool { return s.batch != nil }

func (s *protoSink) Meta(n *repo.Node) error {
	if n.Type == repo.NodeDir {
		s.dirs++
	}
	return s.w.Node(n)
}

func (s *protoSink) File(n *repo.Node, r io.Reader) error {
	if s.dedup() {
		return s.fileDeduped(n, r)
	}
	if err := s.w.Node(n); err != nil {
		return err
	}
	if _, err := s.w.CopyStream(r, s.buf, func(k int) { s.bytes += int64(k) }); err != nil {
		return err
	}
	if err := s.w.NodeEnd(); err != nil {
		return err
	}
	s.files++
	s.report(n.Path)
	return nil
}

// fileDeduped режет файл на стороне сервера и отправляет узел уже с
// готовым списком чанков. Содержимое уходит отдельно и только то,
// которого у десктопа ещё нет.
func (s *protoSink) fileDeduped(n *repo.Node, r io.Reader) error {
	c := repo.NewChunker(r, s.chp)
	var ids []repo.ChunkID
	var size int64
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		id := s.hasher.ID(chunk)
		ids = append(ids, id)
		size += int64(len(chunk))
		s.bytes += int64(len(chunk))
		if err := s.batch.add(id, chunk); err != nil {
			return err
		}
	}
	// Размер берётся фактический: между обходом каталога и чтением файл
	// мог измениться, и записать в дерево устаревшую длину значило бы
	// получить снимок, который не сойдётся при восстановлении.
	n.Chunks, n.Size = ids, size
	s.files++
	s.report(n.Path)
	return s.w.Node(n)
}

func (s *protoSink) Logf(level, format string, args ...any) {
	_ = s.w.Logf(level, format, args...)
}

func (s *protoSink) Progress(path string, bytes int64) {
	s.bytes += bytes
	s.report(path)
}

func (s *protoSink) report(path string) {
	now := time.Now()
	if now.Sub(s.lastReport) < 100*time.Millisecond {
		return
	}
	s.lastReport = now
	p := proto.Progress{Stage: s.stage, Path: path, Files: s.files, Bytes: s.bytes}
	if s.dedup() {
		p.Wire = s.batch.Wire
	}
	_ = s.w.JSON(proto.FrameProgress, p)
}

// Export отдаёт содержимое сервера потоком кадров.
//
// in - обратный канал от десктопа. Нужен только для ответов о том, какие
// чанки уже есть; если дедупликация по сети не запрошена, не читается.
//
// Ошибка отдельного модуля уезжает кадром FrameModule и не прерывает
// поток; фатальной считается только невозможность писать в сам поток -
// то есть обрыв SSH.
func Export(ctx context.Context, out io.Writer, in io.Reader, req *proto.Request, agentVersion string) error {
	var p plan.Plan
	if err := json.Unmarshal(req.Plan, &p); err != nil {
		w := proto.NewWriter(out)
		_ = w.Fatal(fmt.Errorf("autobak: план повреждён: %w", err))
		return err
	}
	// Ограничение скорости накладывается на весь исходящий поток, включая
	// служебные кадры: смысл в том, чтобы не забить канал сервера, а
	// протоколу всё равно, с какой скоростью его читают.
	w := proto.NewWriter(newRateWriter(out, p.BandwidthKBps))
	if err := w.JSON(proto.FrameHello, proto.Hello{
		Version: proto.Version, Agent: agentVersion, Hostname: hostname(),
		OS: runtime.GOOS, Root: os.Geteuid() == 0,
	}); err != nil {
		return err
	}

	if err := p.Validate(); err != nil {
		_ = w.Fatal(err)
		return err
	}

	sink := &protoSink{w: w, buf: make([]byte, proto.MaxDataFrame)}
	if req.WantsWireDedup() {
		key, err := decodeHex(req.ChunkKey)
		if err != nil {
			_ = w.Fatal(err)
			return err
		}
		h, err := repo.NewChunkHasher(key)
		if err != nil {
			_ = w.Fatal(err)
			return err
		}
		sink.hasher, sink.chp = &h, req.Chunker
		sink.batch = &chunkBatch{w: w, in: proto.NewReader(in)}
	}

	for _, pm := range p.Enabled() {
		if err := ctx.Err(); err != nil {
			_ = w.Fatal(err)
			return err
		}
		sink.stage = pm.Kind.Title() + ": " + pm.Name
		baseFiles, baseBytes := sink.files, sink.bytes

		m := repo.Module{Kind: string(pm.Kind), Name: pm.Name}
		c, err := collect.New(pm, p.Excludes)
		if err != nil {
			m.Err = err.Error()
		} else {
			if fc, ok := c.(interface{ SetMaxFileSize(int64) }); ok && p.MaxFileSize > 0 {
				fc.SetMaxFileSize(p.MaxFileSize)
			}
			meta, cerr := c.Collect(ctx, sink)
			m.Meta = meta
			if cerr != nil {
				m.Err = cerr.Error()
			}
		}
		m.Files = sink.files - baseFiles
		m.Bytes = sink.bytes - baseBytes
		if err := w.JSON(proto.FrameModule, m); err != nil {
			return err
		}
	}

	// Остаток пачки обязан уйти до Done: иначе десктоп не досчитается
	// чанков, на которые ссылаются уже отправленные узлы.
	done := proto.Done{Files: sink.files, Dirs: sink.dirs, Bytes: sink.bytes}
	if sink.dedup() {
		if err := sink.batch.flush(); err != nil {
			_ = w.Fatal(err)
			return err
		}
		done.Wire, done.Saved = sink.batch.Wire, sink.batch.Saved
	}
	if err := w.JSON(proto.FrameDone, done); err != nil {
		return err
	}
	return w.Flush()
}

func decodeHex(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		var v int
		for j := range 2 {
			c := s[i+j]
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | int(c-'0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | int(c-'a'+10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | int(c-'A'+10)
			default:
				return nil, errors.New("autobak: ключ идентификаторов не в шестнадцатеричном виде")
			}
		}
		out[i/2] = byte(v)
	}
	return out, nil
}

// --- Сторона десктопа: поток кадров → репозиторий -------------------------

// Import принимает поток от агента и складывает его в репозиторий.
//
// Данные шифруются здесь, на стороне десктопа. В pull-режиме это значит,
// что на сервере нет ни ключа шифрования, ни доступа к хранилищу:
// захватив сервер, к прошлым бэкапам не подобраться никак.
func Import(ctx context.Context, r *repo.Repo, in io.Reader, back io.Writer, opt Options) (*repo.Snapshot, error) {
	start := time.Now()
	// См. Backup: приём данных пишет в репозиторий ровно так же.
	lock, err := r.LockWithRetry(ctx, "приём бэкапа "+opt.Server, false, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	rd := proto.NewReader(in)
	var bw *proto.Writer
	if back != nil {
		bw = proto.NewWriter(back)
	}

	w, err := r.NewWriter(ctx)
	if err != nil {
		return nil, err
	}
	tree := w.NewStream()
	enc := json.NewEncoder(tree)

	var (
		hello    proto.Hello
		modules  []repo.Module
		cur      *repo.Node
		curSW    *repo.StreamWriter
		files    int64
		dirs     int64
		done     bool
		lastPath string
		wire     int64

		// referenced - на какие чанки сослались узлы, received - какие
		// действительно приехали. Сверка в конце: снимок, ссылающийся
		// на непереданный чанк, восстановить будет нечем.
		referenced = map[repo.ChunkID]struct{}{}
		received   = map[repo.ChunkID]struct{}{}
	)

	// Потолки против исчерпания памяти на импортёре. Захваченный сервер
	// шлёт кадры узлов сам, без ограничений сверху, - без этих границ он
	// мог бы раздуть карты до OOM на десктопе/веб-сервере. Значения
	// заведомо выше любого реального бэкапа: 50 млн файлов и 500 млн
	// чанков (~терабайты уникальных данных при среднем чанке 512 КБ).
	const maxNodes = 50_000_000
	const maxChunks = 500_000_000

	// finishNode закрывает узел, содержимое которого приехало потоком
	// (режим без дедупликации по сети).
	finishNode := func() error {
		if cur == nil {
			return nil
		}
		if curSW != nil {
			ids, size, err := curSW.Close()
			if err != nil {
				return err
			}
			cur.Chunks, cur.Size = ids, size
			files++
			curSW = nil
		} else if cur.Type == repo.NodeDir {
			dirs++
		}
		if err := enc.Encode(cur); err != nil {
			return err
		}
		cur = nil
		return nil
	}

	for !done {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, payload, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("autobak: соединение с сервером оборвалось до конца бэкапа")
			}
			return nil, err
		}

		switch t {
		case proto.FrameHello:
			if hello, err = proto.DecodeJSON[proto.Hello](payload); err != nil {
				return nil, err
			}
			if hello.Version != proto.Version {
				return nil, fmt.Errorf(
					"autobak: агент на сервере говорит на версии протокола %d, десктоп ожидает %d - обновите агента",
					hello.Version, proto.Version)
			}

		case proto.FrameNode:
			if err := finishNode(); err != nil {
				return nil, err
			}
			n, err := proto.DecodeJSON[repo.Node](payload)
			if err != nil {
				return nil, err
			}
			lastPath = n.Path
			if files > maxNodes {
				return nil, fmt.Errorf("autobak: слишком много узлов в потоке (>%d) - поток прерван", maxNodes)
			}
			if len(referenced) > maxChunks {
				return nil, fmt.Errorf("autobak: слишком много чанков в потоке (>%d) - поток прерван", maxChunks)
			}
			if len(n.Chunks) > 0 {
				// Узел приехал уже нарезанным: содержимое идёт отдельными
				// кадрами и только то, чего у нас ещё нет.
				for _, id := range n.Chunks {
					referenced[id] = struct{}{}
				}
				files++
				if err := enc.Encode(&n); err != nil {
					return nil, err
				}
				opt.progress(proto.Progress{Stage: "приём", Path: lastPath, Files: files, Wire: wire})
				continue
			}
			cur = &n

		case proto.FrameNeed:
			ids, err := proto.DecodeJSON[[]repo.ChunkID](payload)
			if err != nil {
				return nil, err
			}
			if bw == nil {
				return nil, errors.New("autobak: агент спрашивает о чанках, но обратный канал не открыт")
			}
			ans := proto.Have{Known: make([]bool, len(ids))}
			for i, id := range ids {
				// Отвечаем «есть» только про чанки, которые этот же сервер
				// присылал раньше. Не про весь репозиторий: иначе агент мог
				// бы сослаться на чужой чанк и получить его при следующем
				// восстановлении - то есть прочитать данные другого сервера.
				if _, ok := opt.Known[id]; ok {
					ans.Known[i] = true
					received[id] = struct{}{}
				}
			}
			if err := bw.JSON(proto.FrameHave, ans); err != nil {
				return nil, err
			}
			if err := bw.Flush(); err != nil {
				return nil, err
			}

		case proto.FrameChunkRaw:
			id, data, err := proto.ParseChunkRaw(payload)
			if err != nil {
				return nil, err
			}
			// Проверка обязательна: агент называет чанк сам, и поверить ему
			// на слово значит позволить взломанному серверу подменить
			// содержимое чужого чанка в общем репозитории.
			if got := r.Key().ChunkID(data); got != id {
				return nil, fmt.Errorf(
					"autobak: агент прислал чанк %s, содержимое которого ему не соответствует",
					id.String()[:12])
			}
			if _, _, err := w.WriteChunk(data); err != nil {
				return nil, err
			}
			received[id] = struct{}{}
			wire += int64(len(data))

		case proto.FrameData:
			if cur == nil {
				return nil, errors.New("autobak: данные пришли раньше описания файла - поток повреждён")
			}
			if curSW == nil {
				curSW = w.NewStream()
			}
			if _, err := curSW.Write(payload); err != nil {
				return nil, err
			}
			wire += int64(len(payload))
			opt.progress(proto.Progress{Stage: "приём", Path: lastPath, Files: files, Wire: wire})

		case proto.FrameNodeEnd:
			if err := finishNode(); err != nil {
				return nil, err
			}

		case proto.FrameModule:
			m, err := proto.DecodeJSON[repo.Module](payload)
			if err != nil {
				return nil, err
			}
			if m.Err != "" {
				opt.log("error", m.Name+": "+m.Err)
			}
			modules = append(modules, m)

		case proto.FrameProgress:
			p, err := proto.DecodeJSON[proto.Progress](payload)
			if err == nil {
				opt.progress(p)
			}

		case proto.FrameLog:
			l, err := proto.DecodeJSON[proto.LogMsg](payload)
			if err == nil {
				opt.log(l.Level, l.Msg)
			}

		case proto.FrameError:
			e, _ := proto.DecodeJSON[proto.ErrorMsg](payload)
			return nil, fmt.Errorf("autobak: агент прервал работу: %s", e.Msg)

		case proto.FrameDone:
			if err := finishNode(); err != nil {
				return nil, err
			}
			if d, err := proto.DecodeJSON[proto.Done](payload); err == nil && d.Saved > 0 {
				opt.log("info", fmt.Sprintf("не передавалось повторно: %s",
					repo.HumanBytes(d.Saved)))
			}
			done = true
		}
	}

	// Ни один узел не должен ссылаться на чанк, которого нет.
	for id := range referenced {
		if _, ok := received[id]; ok {
			continue
		}
		if r.Index().Has(id) {
			continue
		}
		return nil, fmt.Errorf(
			"autobak: агент сослался на чанк %s, но не передал его - снимок был бы неполным",
			id.String()[:12])
	}

	treeIDs, _, err := tree.Close()
	if err != nil {
		return nil, err
	}
	stats, err := w.Close()
	if err != nil {
		return nil, err
	}

	snap := &repo.Snapshot{
		Time: start, Server: opt.Server, Hostname: hello.Hostname,
		Parent: opt.Parent, Tags: opt.Tags, Agent: hello.Agent,
		Modules: modules, Tree: treeIDs,
		Stats: repo.SnapshotStats{
			Files: files, Dirs: dirs,
			BytesTotal: stats.BytesTotal, BytesNew: stats.BytesNew,
			BytesStored: stats.BytesStored, BytesWire: wire,
			ChunksTotal: stats.ChunksTotal, ChunksNew: stats.ChunksNew,
			DurationMS: time.Since(start).Milliseconds(),
		},
	}
	// В режиме дедупликации по сети через writer прошло только новое,
	// поэтому полный объём известен агенту, а не нам.
	if snap.Stats.BytesTotal < wire {
		snap.Stats.BytesTotal = wire
	}
	if err := r.SaveSnapshot(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}
