package repo

import (
	"encoding/hex"
	"fmt"
	"time"
)

// Раскладка репозитория:
//
//	config                 открытый заголовок + зашифрованные параметры
//	keys/<keyid>           master key, обёрнутый паролем
//	data/<aa>/<packid>     паки чанков, самоописывающиеся
//	index/<indexid>        зашифрованные индексы (чанк → пак, смещение)
//	snapshots/<snapid>     зашифрованные манифесты снимков
//	locks/<lockid>         блокировки на время записи и prune
const (
	FileConfig   = "config"
	DirKeys      = "keys"
	DirData      = "data"
	DirIndex     = "index"
	DirSnapshots = "snapshots"
	DirLocks     = "locks"

	RepoVersion = 1

	// Потолки на чтение метаданных. Репозиторий может быть чужим или битым,
	// и нельзя позволять ему решать, сколько мы выделим памяти.
	maxConfigSize   = 64 * KiB
	maxKeyFileSize  = 64 * KiB
	maxIndexSize    = 256 * MiB
	maxSnapshotSize = 32 * MiB
	maxTrailerSize  = 64 * MiB
)

// MarshalText делает ChunkID пригодным для JSON и для ключей карт.
func (c ChunkID) MarshalText() ([]byte, error) {
	out := make([]byte, hex.EncodedLen(len(c)))
	hex.Encode(out, c[:])
	return out, nil
}

func (c *ChunkID) UnmarshalText(b []byte) error {
	if len(b) != hex.EncodedLen(len(c)) {
		return fmt.Errorf("autobak: некорректный chunk id длиной %d", len(b))
	}
	_, err := hex.Decode(c[:], b)
	return err
}

// Config - файл config в корне репозитория.
//
// Открытая часть нужна, чтобы понять, что это autobak-репозиторий и каким
// ключом его открывать, ещё не имея ключа. Всё остальное - в Sealed: seed
// чанкера, лежи он открытым, позволил бы по одним лишь размерам чанков
// опознавать известные файлы внутри зашифрованного репозитория.
type Config struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Sealed  []byte    `json:"sealed"`
}

type secretConfig struct {
	Chunker ChunkerParams `json:"chunker"`
}

// NodeType - вид элемента файловой системы. Один символ, потому что дерево
// на миллион файлов хранится как JSONL и каждый байт умножается на миллион.
type NodeType string

const (
	NodeFile    NodeType = "f"
	NodeDir     NodeType = "d"
	NodeSymlink NodeType = "l"
	NodeFIFO    NodeType = "p"
	NodeSocket  NodeType = "s"
	NodeDevice  NodeType = "b"
	NodeChar    NodeType = "c"
)

// Node - один элемент бэкапа.
//
// Дерево целиком пишется в репозиторий как поток JSONL и само режется на
// чанки. Поэтому между соседними снимками оно дедуплицируется наравне с
// данными: список из миллиона файлов, изменившийся в трёх строках, добавит
// к репозиторию килобайты, а не десятки мегабайт.
type Node struct {
	Path   string   `json:"p"`
	Type   NodeType `json:"t"`
	Module string   `json:"mo,omitempty"`

	Mode  uint32 `json:"m"`
	UID   uint32 `json:"u"`
	GID   uint32 `json:"g"`
	User  string `json:"un,omitempty"`
	Group string `json:"gn,omitempty"`

	Size  int64 `json:"sz,omitempty"`
	MTime int64 `json:"mt"`

	Link   string    `json:"ln,omitempty"`
	Dev    uint64    `json:"dev,omitempty"`
	Chunks []ChunkID `json:"c,omitempty"`

	XAttrs map[string]string `json:"x,omitempty"`
	ACL    string            `json:"acl,omitempty"`

	// Err заполняется, если конкретный файл прочитать не удалось. Снимок
	// при этом остаётся валидным: потерять весь бэкап из-за одного файла,
	// который сборщик мусора удалил у нас из-под ног, было бы хуже.
	Err string `json:"err,omitempty"`
}

func (n *Node) ModTime() time.Time { return time.Unix(0, n.MTime) }

// User и Group хранятся именами, а не только числами: на новом сервере
// те же пользователи почти наверняка получат другие uid, и восстановление
// по одним числам расставило бы права случайным людям.

// Module - что именно вошло в снимок.
type Module struct {
	Kind  string         `json:"kind"`
	Name  string         `json:"name"`
	Meta  map[string]any `json:"meta,omitempty"`
	Bytes int64          `json:"bytes"`
	Files int64          `json:"files"`
	// Err означает, что модуль отработал с ошибкой. Снимок всё равно
	// сохраняется: три из четырёх баз лучше, чем ничего, но интерфейс
	// обязан показать такой снимок как неполный.
	Err string `json:"err,omitempty"`
}

func (m Module) OK() bool { return m.Err == "" }

type SnapshotStats struct {
	Files       int64 `json:"files"`
	Dirs        int64 `json:"dirs"`
	BytesTotal  int64 `json:"bytes_total"`
	BytesNew    int64 `json:"bytes_new"`
	BytesStored int64 `json:"bytes_stored"`
	// BytesWire - сколько прошло по сети. В pull-режиме с дедупликацией
	// на стороне сервера заметно меньше BytesTotal.
	BytesWire   int64 `json:"bytes_wire,omitempty"`
	ChunksTotal int64 `json:"chunks_total"`
	ChunksNew   int64 `json:"chunks_new"`
	DurationMS  int64 `json:"duration_ms"`
}

type Snapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Server   string    `json:"server"`
	Hostname string    `json:"hostname"`
	Parent   string    `json:"parent,omitempty"`
	Tags     []string  `json:"tags,omitempty"`
	Agent    string    `json:"agent"`

	Modules []Module      `json:"modules"`
	Stats   SnapshotStats `json:"stats"`

	// Tree - чанки потока JSONL с узлами. Сам список узлов в манифест не
	// кладётся: манифест обязан оставаться маленьким, чтобы список снимков
	// в интерфейсе открывался мгновенно.
	Tree []ChunkID `json:"tree"`

	// Verified - когда снимок последний раз проверялся целиком.
	// Непроверенный бэкап - это ещё не бэкап.
	Verified time.Time `json:"verified,omitempty"`
}

// Complete сообщает, что все модули отработали без ошибок.
func (s *Snapshot) Complete() bool {
	for _, m := range s.Modules {
		if !m.OK() {
			return false
		}
	}
	return true
}

func (s *Snapshot) Failed() []Module {
	var out []Module
	for _, m := range s.Modules {
		if !m.OK() {
			out = append(out, m)
		}
	}
	return out
}
