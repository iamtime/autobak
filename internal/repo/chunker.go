package repo

import "io"

const (
	KiB = 1 << 10
	MiB = 1 << 20
)

// ChunkerParams задаёт границы content-defined chunking.
//
// Seed делает границы чанков непредсказуемыми для того, кто видит только
// репозиторий: без него по одним лишь размерам чанков можно было бы
// опознать известный файл. Seed хранится в config.json репозитория и
// не меняется в течение его жизни - иначе развалится дедупликация.
type ChunkerParams struct {
	Min  int    `json:"min"`
	Avg  int    `json:"avg"`
	Max  int    `json:"max"`
	Seed uint64 `json:"seed"`
}

// DefaultChunkerParams - размеры чанков по умолчанию.
//
// Средние 512 КБ выбраны из наблюдения: стоимость мелкой правки - это
// примерно один чанк, а не доля файла. При среднем чанке в мегабайт дамп
// базы на 2 МБ резался всего на четыре части, и правка одной строки
// стоила четверть дампа. Вчетверо меньший чанк даёт вчетверо более
// точную дедупликацию.
//
// Ниже опускаться не стоит: расход памяти под индекс - около 90 байт на
// чанк, то есть 180 МБ на терабайт данных. Агент на маленьком VPS должен
// в это укладываться.
func DefaultChunkerParams(seed uint64) ChunkerParams {
	return ChunkerParams{Min: 128 * KiB, Avg: 512 * KiB, Max: 2 * MiB, Seed: seed}
}

func (p ChunkerParams) valid() bool {
	return p.Min > 0 && p.Min <= p.Avg && p.Avg <= p.Max && p.Max <= 64*MiB
}

// gearTable детерминированно разворачивает seed в таблицу gear-хэша
// через SplitMix64. Алгоритм зафиксирован навсегда: изменить его -
// значит сделать все существующие репозитории нечитаемыми для дедупа.
func gearTable(seed uint64) *[256]uint64 {
	var t [256]uint64
	x := seed
	for i := range t {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		t[i] = z ^ (z >> 31)
	}
	return &t
}

// cutter - сам алгоритм поиска границы.
//
// Вынесен отдельно, потому что нужен в двух режимах: pull (Chunker тянет
// из io.Reader) и push (StreamWriter принимает байты по мере поступления).
// Дерево файлов пишется вперемежку с содержимым, и без push-режима его
// пришлось бы целиком держать в памяти - на миллионе файлов это сотни
// мегабайт на сервере, где их может не быть.
type cutter struct {
	p                 ChunkerParams
	gear              *[256]uint64
	bitsHigh, bitsLow uint
}

func newCutter(p ChunkerParams) cutter {
	if !p.valid() {
		p = DefaultChunkerParams(p.Seed)
	}
	bits := uint(0)
	for 1<<bits < p.Avg {
		bits++
	}
	// ±2 бита - уровень нормализации из статьи FastCDC.
	return cutter{p: p, gear: gearTable(p.Seed), bitsHigh: bits + 2, bitsLow: bits - 2}
}

// Chunker режет поток на чанки переменной длины по содержимому.
//
// Граница ищется gear-хэшем в нормализованном режиме (FastCDC): до среднего
// размера порог строже, после - мягче. Это поджимает распределение к Avg,
// вместо «геометрического хвоста» из наивного CDC, что заметно уменьшает
// и число мелких чанков, и число упоров в Max.
type Chunker struct {
	cutter
	r          io.Reader
	buf        []byte
	start, end int
	err        error
}

func NewChunker(r io.Reader, p ChunkerParams) *Chunker {
	c := &Chunker{cutter: newCutter(p)}
	c.r = r
	c.buf = make([]byte, 0, 2*c.p.Max)
	return c
}

// Next возвращает следующий чанк. Слайс валиден только до следующего
// вызова Next - вызывающий обязан успеть его обработать или скопировать.
// В конце потока возвращает io.EOF.
func (c *Chunker) Next() ([]byte, error) {
	if c.end-c.start < c.p.Max && c.err == nil {
		c.fill()
	}
	if c.end == c.start {
		if c.err != nil {
			return nil, c.err
		}
		return nil, io.EOF
	}
	n := c.cut(c.buf[c.start:c.end])
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}

func (c *Chunker) fill() {
	if c.start > 0 {
		c.end = copy(c.buf[:cap(c.buf)], c.buf[c.start:c.end])
		c.start = 0
	}
	buf := c.buf[:cap(c.buf)]
	for c.end < len(buf) && c.err == nil {
		n, err := c.r.Read(buf[c.end:])
		c.end += n
		if err != nil {
			c.err = err
		}
	}
	c.buf = buf[:c.end]
}

func (c cutter) cut(data []byte) int {
	n := len(data)
	if n <= c.p.Min {
		return n
	}
	max := min(n, c.p.Max)
	avg := min(c.p.Avg, max)

	var fp uint64
	i := c.p.Min
	for ; i < avg; i++ {
		fp = fp<<1 + c.gear[data[i]]
		if fp>>(64-c.bitsHigh) == 0 {
			return i + 1
		}
	}
	for ; i < max; i++ {
		fp = fp<<1 + c.gear[data[i]]
		if fp>>(64-c.bitsLow) == 0 {
			return i + 1
		}
	}
	return max
}
