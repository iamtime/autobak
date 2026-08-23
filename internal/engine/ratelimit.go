package engine

import (
	"io"
	"time"
)

// rateWriter ограничивает скорость записи.
//
// Нужен на стороне агента: бэкап на сотню гигабайт, запущенный днём,
// способен насытить весь канал сервера, и сайт перестанет отвечать
// раньше, чем кто-то заметит причину. Ограничение скорости обходится
// дешевле, чем объяснения клиенту.
//
// Простое ведро с токенами: перед записью ждём, пока накопится нужное
// количество. Всплеск в одну секунду разрешён - иначе мелкие кадры
// протокола дробились бы на сон между каждым.
type rateWriter struct {
	w     io.Writer
	rate  float64 // байт в секунду
	burst float64
	avail float64
	last  time.Time
}

func newRateWriter(w io.Writer, kbps int) io.Writer {
	if kbps <= 0 {
		return w
	}
	rate := float64(kbps) * 1024
	return &rateWriter{w: w, rate: rate, burst: rate, avail: rate, last: time.Now()}
}

func (r *rateWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		r.refill()
		// Пишем не больше, чем накоплено, но хотя бы что-то: иначе при
		// очень низком лимите цикл крутился бы вхолостую.
		n := int(r.avail)
		if n <= 0 {
			r.sleepFor(1)
			continue
		}
		if n > len(p)-written {
			n = len(p) - written
		}
		k, err := r.w.Write(p[written : written+n])
		written += k
		r.avail -= float64(k)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (r *rateWriter) refill() {
	now := time.Now()
	r.avail += r.rate * now.Sub(r.last).Seconds()
	if r.avail > r.burst {
		r.avail = r.burst
	}
	r.last = now
}

func (r *rateWriter) sleepFor(bytes float64) {
	need := (bytes - r.avail) / r.rate
	if need <= 0 {
		return
	}
	// Спим не дольше секунды за раз: так отмена операции срабатывает
	// быстро, а не через минуту ожидания.
	if need > 1 {
		need = 1
	}
	time.Sleep(time.Duration(need * float64(time.Second)))
}
