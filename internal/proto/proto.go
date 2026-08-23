// Package proto - протокол между агентом на сервере и десктопом.
//
// Работает поверх stdin/stdout процесса, запущенного через SSH. Никаких
// сокетов и портов: живёт ровно столько, сколько живёт SSH-сессия, и
// авторизуется тем же SSH-ключом.
//
// Поток однонаправленный и последовательный: агент шлёт узел, затем его
// содержимое, затем следующий узел. Никаких запросов «дай файл X» -
// иначе сайт из 200 тысяч мелких файлов означал бы 200 тысяч круговых
// задержек, что на канале с пингом 30 мс превращается в полтора часа
// чистого ожидания.
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/iamtime/autobak/internal/repo"
)

const (
	Version = 2

	// MaxDataFrame - размер куска содержимого файла.
	MaxDataFrame = 1 << 20
	// maxMetaFrame ограничивает служебные кадры. Узел с очень длинным
	// путём и списком xattr всё равно укладывается на порядки меньше.
	maxMetaFrame = 8 << 20
)

type FrameType byte

const (
	FrameHello    FrameType = 1  // первый кадр агента: версия, ОС, имя хоста
	FrameNode     FrameType = 2  // метаданные очередного объекта
	FrameData     FrameType = 3  // кусок содержимого текущего узла
	FrameNodeEnd  FrameType = 4  // содержимое закончилось
	FrameProgress FrameType = 5  // счётчики для интерфейса
	FrameLog      FrameType = 6  // сообщение в журнал
	FrameModule   FrameType = 7  // итог по модулю (в том числе с ошибкой)
	FrameError    FrameType = 8  // фатальная ошибка, поток прерван
	FrameDone     FrameType = 9  // поток завершён штатно
	FrameRequest  FrameType = 10 // десктоп → агент: план и ключ идентификаторов
	FrameAck      FrameType = 11
	FrameChunks   FrameType = 12 // агент → десктоп: список чанков текущего узла
	FrameNeed     FrameType = 13 // агент → десктоп: какие из этих уже есть?
	FrameHave     FrameType = 14 // десктоп → агент: ответ маской
	FrameChunkRaw FrameType = 15 // агент → десктоп: 32 байта id, затем содержимое
)

func (t FrameType) String() string {
	switch t {
	case FrameHello:
		return "hello"
	case FrameNode:
		return "node"
	case FrameData:
		return "data"
	case FrameNodeEnd:
		return "node-end"
	case FrameProgress:
		return "progress"
	case FrameLog:
		return "log"
	case FrameModule:
		return "module"
	case FrameError:
		return "error"
	case FrameDone:
		return "done"
	case FrameRequest:
		return "request"
	case FrameAck:
		return "ack"
	case FrameChunks:
		return "chunks"
	case FrameNeed:
		return "need"
	case FrameHave:
		return "have"
	case FrameChunkRaw:
		return "chunk"
	}
	return fmt.Sprintf("frame(%d)", byte(t))
}

// --- Полезные нагрузки ----------------------------------------------------

// Request - то, что десктоп посылает агенту перед выгрузкой.
//
// ChunkKey превращает передачу в инкрементальную: агент режет файлы теми
// же границами, называет чанки теми же именами и спрашивает, какие из них
// уже есть. По сети едет только новое.
//
// Ключ здесь один из четырёх - тот, которым считаются идентификаторы.
// Расшифровать им нельзя ничего: данные закрыты другим ключом, который
// сервер не получает никогда.
type Request struct {
	Plan json.RawMessage `json:"plan"`

	ChunkKey string             `json:"chunk_key,omitempty"`
	Chunker  repo.ChunkerParams `json:"chunker,omitempty"`
}

// WantsWireDedup - просит ли десктоп не передавать уже известное.
func (r *Request) WantsWireDedup() bool { return r.ChunkKey != "" }

// Have - ответ на Need: для каждого запрошенного идентификатора говорит,
// известен ли он. Порядок совпадает с порядком в запросе.
type Have struct {
	Known []bool `json:"known"`
}

type Hello struct {
	Version  int    `json:"version"`
	Agent    string `json:"agent"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Root     bool   `json:"root"`
}

type Progress struct {
	Stage      string `json:"stage"`
	Path       string `json:"path,omitempty"`
	Files      int64  `json:"files"`
	Bytes      int64  `json:"bytes"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
	// Wire - сколько ушло по сети. Отличается от Bytes на всё, что
	// не пришлось передавать повторно, и именно это число интересно
	// человеку с медленным каналом.
	Wire int64 `json:"wire,omitempty"`
}

type LogMsg struct {
	Level string `json:"level"` // info | warn | error
	Msg   string `json:"msg"`
}

type Done struct {
	Files int64 `json:"files"`
	Dirs  int64 `json:"dirs"`
	Bytes int64 `json:"bytes"`
	Wire  int64 `json:"wire,omitempty"`
	Saved int64 `json:"saved,omitempty"`
}

type ErrorMsg struct {
	Msg string `json:"msg"`
}

// --- Запись ---------------------------------------------------------------

type Writer struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	hdr [5]byte
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{bw: bufio.NewWriterSize(w, 256<<10)}
}

func (w *Writer) Frame(t FrameType, payload []byte) error {
	if len(payload) > maxMetaFrame {
		return fmt.Errorf("autobak: кадр %s слишком велик (%d байт)", t, len(payload))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hdr[0] = byte(t)
	binary.BigEndian.PutUint32(w.hdr[1:], uint32(len(payload)))
	if _, err := w.bw.Write(w.hdr[:]); err != nil {
		return err
	}
	_, err := w.bw.Write(payload)
	return err
}

func (w *Writer) JSON(t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.Frame(t, b)
}

// Node открывает новый объект. За ним идут кадры Data и затем NodeEnd.
func (w *Writer) Node(n *repo.Node) error { return w.JSON(FrameNode, n) }

func (w *Writer) Data(b []byte) error { return w.Frame(FrameData, b) }

func (w *Writer) NodeEnd() error { return w.Frame(FrameNodeEnd, nil) }

// ChunkRaw отправляет содержимое чанка: 32 байта идентификатора, затем
// сами данные. Идентификатор передаётся вместе с содержимым, чтобы
// принимающая сторона могла проверить его пересчётом, а не поверить
// на слово порядку кадров.
func (w *Writer) ChunkRaw(id repo.ChunkID, data []byte) error {
	buf := make([]byte, len(id)+len(data))
	copy(buf, id[:])
	copy(buf[len(id):], data)
	return w.Frame(FrameChunkRaw, buf)
}

// ParseChunkRaw разбирает кадр с содержимым чанка.
func ParseChunkRaw(payload []byte) (repo.ChunkID, []byte, error) {
	var id repo.ChunkID
	if len(payload) < len(id) {
		return id, nil, fmt.Errorf("autobak: кадр чанка короче идентификатора")
	}
	copy(id[:], payload[:len(id)])
	return id, payload[len(id):], nil
}

func (w *Writer) Logf(level, format string, args ...any) error {
	return w.JSON(FrameLog, LogMsg{Level: level, Msg: fmt.Sprintf(format, args...)})
}

// Fatal сообщает об ошибке, после которой поток продолжать нельзя.
// Читающая сторона обязана отличать её от ошибки отдельного модуля.
func (w *Writer) Fatal(err error) error {
	if e := w.JSON(FrameError, ErrorMsg{Msg: err.Error()}); e != nil {
		return e
	}
	return w.Flush()
}

func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bw.Flush()
}

// CopyStream перекладывает содержимое файла кадрами Data.
//
// Буфер один на весь вызов, поэтому расход памяти не зависит от размера
// файла: дамп базы на 200 ГБ проходит через тот же мегабайт.
func (w *Writer) CopyStream(r io.Reader, buf []byte, onBytes func(int)) (int64, error) {
	if buf == nil {
		buf = make([]byte, MaxDataFrame)
	}
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := w.Data(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if onBytes != nil {
				onBytes(n)
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// --- Чтение ---------------------------------------------------------------

type Reader struct {
	br  *bufio.Reader
	hdr [5]byte
	buf []byte
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 256<<10)}
}

// Next читает следующий кадр. Возвращаемый срез валиден до следующего
// вызова: копировать его - забота вызывающего.
func (r *Reader) Next() (FrameType, []byte, error) {
	if _, err := io.ReadFull(r.br, r.hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	t := FrameType(r.hdr[0])
	n := int(binary.BigEndian.Uint32(r.hdr[1:]))
	if n > maxMetaFrame {
		return 0, nil, fmt.Errorf("autobak: получен кадр %s размером %d байт - поток повреждён", t, n)
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	r.buf = r.buf[:n]
	if n > 0 {
		if _, err := io.ReadFull(r.br, r.buf); err != nil {
			return 0, nil, err
		}
	}
	return t, r.buf, nil
}

func DecodeJSON[T any](payload []byte) (T, error) {
	var v T
	err := json.Unmarshal(payload, &v)
	return v, err
}
