package repo

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Сжатие идёт до шифрования: после шифрования данные неотличимы от шума
// и не жмутся вовсе. Дампы БД и исходники сайтов ужимаются в 4–8 раз,
// то есть это самая дешёвая экономия и трафика, и места в хранилище.
var (
	zEnc *zstd.Encoder
	zDec *zstd.Decoder
)

func init() {
	var err error
	// Уровень 3 - сознательный компромисс: на дампах даёт почти столько же,
	// сколько уровень 9, но не превращает бэкап в тест процессора.
	zEnc, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic("autobak: zstd encoder: " + err.Error())
	}
	zDec, err = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		// Потолок распаковки - вдвое больше максимального чанка. Это защита
		// от zip-бомбы в подменённом репозитории, а не оптимизация памяти.
		zstd.WithDecoderMaxMemory(uint64(8*MiB)))
	if err != nil {
		panic("autobak: zstd decoder: " + err.Error())
	}
}

// compress возвращает сжатый чанк и признак того, что сжатие применено.
//
// Если выигрыш меньше 3%, отдаём исходные данные: на уже сжатом (jpg, zip,
// mp4, а это заметная часть любого сайта) zstd тратит время впустую и иногда
// даёт результат крупнее оригинала.
func compress(plain []byte) ([]byte, bool) {
	out := zEnc.EncodeAll(plain, make([]byte, 0, len(plain)))
	if len(out) >= len(plain)-len(plain)/32 {
		return plain, false
	}
	return out, true
}

func decompress(in []byte, plainLen int) ([]byte, error) {
	out, err := zDec.DecodeAll(in, make([]byte, 0, plainLen))
	if err != nil {
		return nil, fmt.Errorf("autobak: чанк не распаковывается: %w", err)
	}
	if len(out) != plainLen {
		return nil, fmt.Errorf("autobak: после распаковки %d байт вместо %d", len(out), plainLen)
	}
	return out, nil
}
