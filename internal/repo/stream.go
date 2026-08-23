package repo

import "io"

// StreamWriter принимает байты и нарезает их на чанки по мере поступления.
//
// В отличие от Chunker, который сам тянет из io.Reader, здесь данные
// приходят снаружи. Это нужно для дерева файлов: движок записывает
// содержимое очередного файла, узнаёт список его чанков и только тогда
// может дописать строку с описанием узла. Тянуть такое из io.Reader
// пришлось бы через горутину с io.Pipe, а значит - параллельно с записью
// содержимого, чего Writer не допускает.
type StreamWriter struct {
	cutter
	w    *Writer
	buf  []byte
	ids  []ChunkID
	size int64
	err  error
}

func (w *Writer) NewStream() *StreamWriter {
	sw := &StreamWriter{cutter: newCutter(w.r.chp), w: w}
	sw.buf = make([]byte, 0, 2*sw.p.Max)
	return sw
}

func (sw *StreamWriter) Write(p []byte) (int, error) {
	if sw.err != nil {
		return 0, sw.err
	}
	sw.buf = append(sw.buf, p...)
	sw.size += int64(len(p))

	// Резать можно только когда накоплено не меньше максимального чанка:
	// иначе граница, найденная в неполном окне, зависела бы от размера
	// поступившей порции, а не от содержимого, и дедупликация развалилась
	// бы при любом изменении размера буферов.
	for len(sw.buf) >= sw.p.Max {
		n := sw.cut(sw.buf[:sw.p.Max])
		if err := sw.emit(sw.buf[:n]); err != nil {
			sw.err = err
			return 0, err
		}
		// Остаток сдвигается в начало того же массива: копия не больше
		// максимального чанка, а массив не растёт бесконечно.
		sw.buf = append(sw.buf[:0], sw.buf[n:]...)
	}
	return len(p), nil
}

func (sw *StreamWriter) emit(chunk []byte) error {
	id, _, err := sw.w.WriteChunk(chunk)
	if err != nil {
		return err
	}
	sw.ids = append(sw.ids, id)
	return nil
}

// Close дорезает остаток и возвращает список чанков потока.
func (sw *StreamWriter) Close() ([]ChunkID, int64, error) {
	if sw.err != nil {
		return nil, 0, sw.err
	}
	for len(sw.buf) > 0 {
		n := sw.cut(sw.buf)
		if err := sw.emit(sw.buf[:n]); err != nil {
			return nil, 0, err
		}
		sw.buf = append(sw.buf[:0], sw.buf[n:]...)
	}
	return sw.ids, sw.size, nil
}

// WriteStream режет поток на чанки и складывает их в репозиторий.
//
// Это единственный способ попасть данным в репозиторий: и файлы сайта, и
// вывод mysqldump, и дерево файлов проходят через него. Ничего не
// буферизуется целиком - поток любого размера обрабатывается в постоянной
// памяти, поэтому дамп базы на 200 ГБ не требует ни места на диске сервера,
// ни памяти под него.
func (w *Writer) WriteStream(r io.Reader) ([]ChunkID, int64, error) {
	sw := w.NewStream()
	buf := make([]byte, 512<<10)
	if _, err := io.CopyBuffer(sw, r, buf); err != nil {
		return nil, 0, err
	}
	return sw.Close()
}
