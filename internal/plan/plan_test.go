package plan

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	p := New("prod")
	if err := p.Validate(); !errors.Is(err, ErrEmptyPlan) {
		t.Fatalf("пустой план принят: %v", err)
	}

	p.Modules = []Module{{Kind: KindFiles, Name: "site", Enabled: true, Paths: []string{"/var/www"}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}

	bad := []struct {
		name string
		m    Module
	}{
		{"без имени", Module{Kind: KindFiles, Enabled: true, Paths: []string{"/x"}}},
		{"без путей", Module{Kind: KindFiles, Name: "x", Enabled: true}},
		{"относительный путь", Module{Kind: KindFiles, Name: "x", Enabled: true, Paths: []string{"var/www"}}},
		{"переход вверх", Module{Kind: KindFiles, Name: "x", Enabled: true, Paths: []string{"/var/../etc"}}},
		{"неизвестный тип", Module{Kind: "магия", Name: "x", Enabled: true}},
	}
	for _, c := range bad {
		p := New("prod")
		p.Modules = []Module{c.m}
		if err := p.Validate(); err == nil {
			t.Errorf("принят некорректный модуль: %s", c.name)
		}
	}

	// Выключенные модули не проверяются: человек мог отключить модуль
	// именно потому, что путь к нему больше не существует.
	p2 := New("prod")
	p2.Modules = []Module{
		{Kind: KindFiles, Name: "живой", Enabled: true, Paths: []string{"/var/www"}},
		{Kind: "мусор", Enabled: false},
	}
	if err := p2.Validate(); err != nil {
		t.Fatalf("выключенный модуль помешал проверке: %v", err)
	}
}

func TestNiceRange(t *testing.T) {
	p := New("prod")
	p.Modules = []Module{{Kind: KindMySQL, Name: "mysql", Enabled: true}}
	p.Nice = 25
	if err := p.Validate(); err == nil {
		t.Fatal("недопустимое значение nice принято")
	}
}

func TestEnabledAndDescribe(t *testing.T) {
	p := New("prod")
	p.Modules = []Module{
		{Kind: KindFiles, Name: "site.ru", Enabled: true, Paths: []string{"/var/www"}},
		{Kind: KindMySQL, Name: "MySQL", Enabled: true},
		{Kind: KindDocker, Name: "Docker", Enabled: false},
	}
	if len(p.Enabled()) != 2 {
		t.Fatalf("включённых модулей: %d", len(p.Enabled()))
	}
	d := p.Describe()
	if !strings.Contains(d, "site.ru") || !strings.Contains(d, "MySQL") || strings.Contains(d, "Docker") {
		t.Fatalf("описание плана неверно: %s", d)
	}
}

// Умолчания должны отсекать то, что занимает основной объём типового
// сайта и при этом восстанавливается одной командой.
func TestDefaultExcludesCoverCommonJunk(t *testing.T) {
	want := []string{"node_modules", "cache", "*.log", "*.sock"}
	all := strings.Join(DefaultExcludes(), "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("в умолчаниях нет исключения для %q", w)
		}
	}
	// vendor целиком исключать нельзя: без него сайт на PHP не заработает,
	// а composer install требует сети и совпадения версий.
	for _, l := range DefaultExcludes() {
		if l == "**/vendor" {
			t.Error("каталог vendor исключён целиком - сайт не восстановится")
		}
	}
}

// Регрессия на находку аудита: серверный allowlist должен ловить попытку
// украденного ключа выгрузить файл вне разрешённых каталогов.
func TestCheckAllowed(t *testing.T) {
	allow := ParseAllow([]string{"--allow=/home,/var/www"})
	if allow == nil {
		t.Fatal("ParseAllow вернул nil при непустом --allow")
	}

	ok := New("prod")
	ok.Modules = []Module{{Kind: KindFiles, Name: "site", Enabled: true, Paths: []string{"/var/www/site"}}}
	if err := ok.CheckAllowed(allow); err != nil {
		t.Fatalf("путь внутри разрешённого корня отклонён: %v", err)
	}

	// Классическая атака: план требует ключ репозитория.
	evil := New("prod")
	evil.Modules = []Module{{Kind: KindFiles, Name: "x", Enabled: true, Paths: []string{"/etc/autobak/key"}}}
	if err := evil.CheckAllowed(allow); err == nil {
		t.Fatal("выгрузка /etc/autobak/key вне --allow не отклонена")
	}

	// Обход через похожий префикс не должен проходить.
	sneaky := New("prod")
	sneaky.Modules = []Module{{Kind: KindFiles, Name: "x", Enabled: true, Paths: []string{"/var/www-secret"}}}
	if err := sneaky.CheckAllowed(allow); err == nil {
		t.Fatal("/var/www-secret ошибочно засчитан как /var/www")
	}

	// Базы запрещены, пока не задан --allow-db.
	db := New("prod")
	db.Modules = []Module{{Kind: KindMySQL, Name: "db", Enabled: true}}
	if err := db.CheckAllowed(allow); err == nil {
		t.Fatal("модуль mysql прошёл без --allow-db")
	}
	if err := db.CheckAllowed(ParseAllow([]string{"--allow-db"})); err != nil {
		t.Fatalf("модуль mysql отклонён при --allow-db: %v", err)
	}

	// Без ограничения (nil) - всё разрешено, как в локальном push-режиме.
	if err := evil.CheckAllowed(nil); err != nil {
		t.Fatalf("nil-ограничение должно всё пропускать: %v", err)
	}
}

// AllowForPlan выводит из плана разрешение, вписываемое в authorized_keys.
func TestAllowForPlan(t *testing.T) {
	p := New("prod")
	p.Modules = []Module{
		{Kind: KindFiles, Name: "site", Enabled: true, Paths: []string{"/var/www"}},
		{Kind: KindMySQL, Name: "db", Enabled: true},
		{Kind: KindFiles, Name: "off", Enabled: false, Paths: []string{"/secret"}},
	}
	a := AllowForPlan(p)
	if len(a.Roots) != 1 || a.Roots[0] != "/var/www" {
		t.Fatalf("корни выведены неверно: %v", a.Roots)
	}
	if !a.AllowDB {
		t.Fatal("модуль mysql должен включить AllowDB")
	}
	// Выведенное разрешение обязано пропускать сам план.
	if err := p.CheckAllowed(a); err != nil {
		t.Fatalf("план не проходит собственное выведенное ограничение: %v", err)
	}
	args := a.Args()
	if len(args) != 2 {
		t.Fatalf("Args() = %v, ожидалось --allow= и --allow-db", args)
	}
}
