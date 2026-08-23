package collect

import (
	"path"
	"strings"
)

// Matcher решает, пропустить ли путь.
//
// Поддерживаются три формы, потому что именно их люди и пишут:
//
//	*.log              - по имени файла, где угодно
//	**/node_modules    - каталог с таким именем на любой глубине
//	/var/log/**        - всё под конкретным путём
//
// Совпадение с каталогом отсекает и всё его содержимое: обходчик не
// заходит внутрь, поэтому исключение node_modules экономит не только место,
// но и десятки тысяч бесполезных обращений к диску.
type Matcher struct {
	base   []string // без слэшей: сравнивается с именем файла
	anySeg []string // **/... : сравнивается с любым суффиксом пути
	prefix []string // /path/** : префикс
	exact  []string // /path или /path/*.conf
}

func NewMatcher(patterns ...string) *Matcher {
	m := &Matcher{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		p = strings.TrimSuffix(p, "/")
		switch {
		case !strings.Contains(p, "/"):
			m.base = append(m.base, p)
		case strings.HasPrefix(p, "**/"):
			m.anySeg = append(m.anySeg, strings.TrimPrefix(p, "**/"))
		case strings.HasSuffix(p, "/**"):
			m.prefix = append(m.prefix, strings.TrimSuffix(p, "/**"))
		case !strings.HasPrefix(p, "/"):
			// Относительный путь со слэшем ведёт себя как **/путь:
			// человек, написавший "storage/logs", имеет в виду именно это.
			m.anySeg = append(m.anySeg, p)
		default:
			m.exact = append(m.exact, p)
		}
	}
	return m
}

func (m *Matcher) Empty() bool {
	return len(m.base)+len(m.anySeg)+len(m.prefix)+len(m.exact) == 0
}

// Match принимает абсолютный путь в slash-форме.
func (m *Matcher) Match(p string) bool {
	if m == nil || m.Empty() {
		return false
	}
	name := path.Base(p)
	for _, g := range m.base {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	for _, g := range m.anySeg {
		if matchSuffix(g, p) {
			return true
		}
	}
	for _, pre := range m.prefix {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	for _, e := range m.exact {
		if p == e || strings.HasPrefix(p, e+"/") {
			return true
		}
		if ok, _ := path.Match(e, p); ok {
			return true
		}
	}
	return false
}

// matchSuffix ищет шаблон среди подряд идущих сегментов пути.
//
// Сравнение идёт окнами по числу сегментов в шаблоне, поэтому
// "storage/logs" совпадёт и с самим каталогом /home/web/storage/logs,
// и с файлом внутри него, но не с /home/web/mystorage/logs - граница
// сегмента соблюдается, чего не даёт обычное вхождение подстроки.
func matchSuffix(pattern, p string) bool {
	n := strings.Count(pattern, "/") + 1
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for start := 0; start+n <= len(segs); start++ {
		if ok, _ := path.Match(pattern, strings.Join(segs[start:start+n], "/")); ok {
			return true
		}
	}
	return false
}
