package repo

import (
	"errors"
	"fmt"
	"time"
)

// Retention - политика хранения по схеме «дед-отец-сын».
//
// Нули означают «этот интервал не удерживает ничего». Пустая политика
// целиком не удаляет ничего: молча стереть все снимки из-за незаполненной
// формы - самая дорогая ошибка, которую эта программа могла бы совершить.
type Retention struct {
	Last    int `json:"last"`    // столько последних снимков независимо от дат
	Hourly  int `json:"hourly"`  //
	Daily   int `json:"daily"`   //
	Weekly  int `json:"weekly"`  //
	Monthly int `json:"monthly"` //
	Yearly  int `json:"yearly"`  //

	// KeepWithin удерживает всё моложе указанного срока, что бы ни говорили
	// счётчики выше. Страховка от «почистил и обнаружил, что вчерашнего нет».
	KeepWithin Duration `json:"keep_within"`
}

// Duration - time.Duration, который в JSON выглядит как "720h", а не как
// число наносекунд: конфиги эти человек читает и правит руками.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("autobak: некорректный срок %q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

func DefaultRetention() Retention {
	return Retention{
		Last: 3, Daily: 14, Weekly: 8, Monthly: 12, Yearly: 3,
		KeepWithin: Duration(48 * time.Hour),
	}
}

func (p Retention) empty() bool {
	return p.Last == 0 && p.Hourly == 0 && p.Daily == 0 &&
		p.Weekly == 0 && p.Monthly == 0 && p.Yearly == 0 && p.KeepWithin == 0
}

func (p Retention) Describe() string {
	if p.empty() {
		return "хранить всё"
	}
	parts := []struct {
		n int
		s string
	}{
		{p.Last, "последних"}, {p.Hourly, "часовых"}, {p.Daily, "дневных"},
		{p.Weekly, "недельных"}, {p.Monthly, "месячных"}, {p.Yearly, "годовых"},
	}
	out := ""
	for _, x := range parts {
		if x.n > 0 {
			if out != "" {
				out += ", "
			}
			out += fmt.Sprintf("%d %s", x.n, x.s)
		}
	}
	if p.KeepWithin > 0 {
		if out != "" {
			out += "; "
		}
		out += "всё моложе " + time.Duration(p.KeepWithin).String()
	}
	return out
}

// Apply делит снимки на оставляемые и удаляемые.
//
// Политика применяется отдельно к каждому серверу: у них разные расписания
// и разное число снимков, и общий счётчик «14 дневных» вымыл бы историю
// того сервера, который бэкапится реже.
func (p Retention) Apply(snaps []*Snapshot, now time.Time) (keep, remove []*Snapshot) {
	if p.empty() {
		return append([]*Snapshot(nil), snaps...), nil
	}
	byServer := map[string][]*Snapshot{}
	order := []string{}
	for _, s := range snaps {
		if _, ok := byServer[s.Server]; !ok {
			order = append(order, s.Server)
		}
		byServer[s.Server] = append(byServer[s.Server], s)
	}
	for _, srv := range order {
		k, r := p.applyOne(byServer[srv], now)
		keep = append(keep, k...)
		remove = append(remove, r...)
	}
	return keep, remove
}

func (p Retention) applyOne(snaps []*Snapshot, now time.Time) (keep, remove []*Snapshot) {
	list := append([]*Snapshot(nil), snaps...)
	sortSnapshotsDesc(list)

	kept := map[string]bool{}
	buckets := []struct {
		limit int
		key   func(time.Time) string
	}{
		{p.Hourly, func(t time.Time) string { return t.Format("2006-01-02 15") }},
		{p.Daily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{p.Weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-w%d", y, w) }},
		{p.Monthly, func(t time.Time) string { return t.Format("2006-01") }},
		{p.Yearly, func(t time.Time) string { return t.Format("2006") }},
	}
	seen := make([]map[string]bool, len(buckets))
	for i := range seen {
		seen[i] = map[string]bool{}
	}

	for i, s := range list {
		if i < p.Last {
			kept[s.ID] = true
		}
		if p.KeepWithin > 0 && now.Sub(s.Time) <= time.Duration(p.KeepWithin) {
			kept[s.ID] = true
		}
		// Границы считаются в местном времени: «дневной снимок» должен
		// совпадать с календарным днём пользователя, а не с днём по UTC.
		t := s.Time.Local()
		for bi, b := range buckets {
			if len(seen[bi]) >= b.limit {
				continue
			}
			k := b.key(t)
			if !seen[bi][k] {
				seen[bi][k] = true
				kept[s.ID] = true
			}
		}
	}

	for _, s := range list {
		if kept[s.ID] {
			keep = append(keep, s)
		} else {
			remove = append(remove, s)
		}
	}
	// Последний снимок сервера не удаляется никогда, каким бы старым он ни
	// был: пустая история - это не результат применения политики хранения.
	if len(keep) == 0 && len(remove) > 0 {
		keep = append(keep, remove[0])
		remove = remove[1:]
	}
	return keep, remove
}

var ErrWouldDeleteAll = errors.New("autobak: политика хранения удалила бы все снимки - операция отменена")
