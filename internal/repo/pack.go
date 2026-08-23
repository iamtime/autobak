package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/iamtime/autobak/internal/backend"
)

// Формат пака:
//
//	[блоб 0][блоб 1] ... [блоб N-1][трейлер][длина трейлера uint32][ABP1]
//
// Мелкие чанки складываются в паки по ~16 МБ, иначе репозиторий на 100 ГБ
// превратился бы в сотню тысяч объектов: в S3 это сотня тысяч оплаченных
// запросов и мучительно долгий листинг.
//
// Трейлер повторяет содержимое индекса для своего пака. Это дублирование
// намеренное: потеряв или испортив каталог index/, репозиторий можно
// полностью восстановить, прочитав хвосты паков.
const (
	packMagic      = "ABP1"
	packTrailerLen = 4 + len(packMagic)
	packTargetSize = 16 * MiB
)

type blobEntry struct {
	ID   ChunkID `json:"i"`
	Off  uint64  `json:"o"`
	Len  uint32  `json:"l"`           // длина зашифрованного блоба
	PLen uint32  `json:"p"`           // длина исходного чанка
	Comp bool    `json:"z,omitempty"` // сжат ли перед шифрованием
}

func newPackID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("autobak: нет источника случайности: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// packName раскладывает паки по подкаталогам из двух hex-символов: 256
// каталогов вместо одного с десятками тысяч файлов, иначе ext4 и NTFS
// начинают заметно тормозить на листинге.
func packName(id string) string { return DirData + "/" + id[:2] + "/" + id }

type packBuilder struct {
	id      string
	buf     bytes.Buffer
	entries []blobEntry
}

func newPackBuilder() (*packBuilder, error) {
	id, err := newPackID()
	if err != nil {
		return nil, err
	}
	p := &packBuilder{id: id}
	p.buf.Grow(packTargetSize + MiB)
	return p, nil
}

// add шифрует чанк и кладёт его в пак. Возвращает число записанных байт.
func (p *packBuilder) add(key *MasterKey, id ChunkID, plain []byte) (int, error) {
	payload, comp := compress(plain)
	sealed, err := key.SealChunk(id, payload)
	if err != nil {
		return 0, err
	}
	p.entries = append(p.entries, blobEntry{
		ID:   id,
		Off:  uint64(p.buf.Len()),
		Len:  uint32(len(sealed)),
		PLen: uint32(len(plain)),
		Comp: comp,
	})
	p.buf.Write(sealed)
	return len(sealed), nil
}

func (p *packBuilder) size() int  { return p.buf.Len() }
func (p *packBuilder) count() int { return len(p.entries) }

func (p *packBuilder) finish(key *MasterKey) ([]byte, error) {
	meta, err := json.Marshal(p.entries)
	if err != nil {
		return nil, err
	}
	trailer, err := key.SealMeta(packName(p.id), meta)
	if err != nil {
		return nil, err
	}
	if len(trailer) > maxTrailerSize {
		return nil, fmt.Errorf("autobak: трейлер пака %s неправдоподобно велик", p.id)
	}
	p.buf.Write(trailer)
	binary.Write(&p.buf, binary.BigEndian, uint32(len(trailer)))
	p.buf.WriteString(packMagic)
	return p.buf.Bytes(), nil
}

// readPackTrailer восстанавливает список чанков пака, прочитав только его
// хвост. Три запроса вместо скачивания 16 МБ - на этом стоит и repair,
// и проверка целостности.
func readPackTrailer(ctx context.Context, be backend.Backend, key *MasterKey, id string) ([]blobEntry, error) {
	name := packName(id)
	st, err := be.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	if st.Size < int64(packTrailerLen) {
		return nil, fmt.Errorf("autobak: пак %s обрезан", id)
	}
	foot, err := readRange(ctx, be, name, st.Size-int64(packTrailerLen), int64(packTrailerLen))
	if err != nil {
		return nil, err
	}
	if string(foot[4:]) != packMagic {
		return nil, fmt.Errorf("autobak: %s не похож на пак autobak", id)
	}
	tlen := int64(binary.BigEndian.Uint32(foot[:4]))
	if tlen <= 0 || tlen > maxTrailerSize || tlen > st.Size-int64(packTrailerLen) {
		return nil, fmt.Errorf("autobak: у пака %s повреждён трейлер", id)
	}
	raw, err := readRange(ctx, be, name, st.Size-int64(packTrailerLen)-tlen, tlen)
	if err != nil {
		return nil, err
	}
	meta, err := key.OpenMeta(name, raw)
	if err != nil {
		return nil, fmt.Errorf("autobak: пак %s: %w", id, err)
	}
	var entries []blobEntry
	if err := json.Unmarshal(meta, &entries); err != nil {
		return nil, fmt.Errorf("autobak: пак %s: не разобрать трейлер: %w", id, err)
	}
	return entries, nil
}

func readRange(ctx context.Context, be backend.Backend, name string, off, length int64) ([]byte, error) {
	rc, err := be.GetRange(ctx, name, off, length)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, length)
	if _, err := io.ReadFull(rc, buf); err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return nil, fmt.Errorf("autobak: %s: %w", name, backend.ErrTruncated)
		}
		return nil, err
	}
	return buf, nil
}
