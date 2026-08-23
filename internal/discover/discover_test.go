package discover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/plan"
)

func TestParseHestiaLine(t *testing.T) {
	line := `DOMAIN='shop.ru' IP='203.0.113.10' ALIAS='www.shop.ru' SSL='yes' ` +
		`BACKEND_TEMPLATE='PHP-8_2' DOCUMENT_ROOT='' SUSPENDED='no'`
	kv := parseHestiaLine(line)
	if kv["DOMAIN"] != "shop.ru" || kv["SSL"] != "yes" || kv["ALIAS"] != "www.shop.ru" {
		t.Fatalf("строка разобрана неверно: %+v", kv)
	}
	// Пустое значение должно остаться пустым, а не пропасть вместе с ключом.
	if v, ok := kv["DOCUMENT_ROOT"]; !ok || v != "" {
		t.Fatalf("пустое значение потеряно: %+v", kv)
	}
	if parseHestiaLine("просто строка") != nil {
		t.Fatal("строка без пар ключ-значение разобрана как запись")
	}
}

func TestPHPFromTemplate(t *testing.T) {
	cases := map[string]string{
		"PHP-8_2":        "8.2",
		"php-7_4":        "7.4",
		"default":        "",
		"":               "",
		"PHP-8_3-apache": "8.3-APACHE",
	}
	for in, want := range cases {
		if got := phpFromTemplate(in); got != want {
			t.Errorf("phpFromTemplate(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// Связь базы с сайтом - подсказка для интерфейса, а не факт. Но подсказка
// должна быть осмысленной: иначе к сайту прилипнут чужие базы.
func TestDBLooksRelated(t *testing.T) {
	yes := [][3]string{
		{"admin_shop", "admin", "shop.ru"},
		{"admin_shop_prod", "admin", "shop.ru"},
		{"admin_blog", "admin", "blog-site.ru"},
	}
	for _, c := range yes {
		if !dbLooksRelated(c[0], c[1], c[2]) {
			t.Errorf("база %s не связалась с сайтом %s", c[0], c[2])
		}
	}
	no := [][3]string{
		{"admin_forum", "admin", "shop.ru"},
		{"other_shop", "admin", "shop.ru"}, // чужой пользователь
		{"shop", "admin", "shop.ru"},       // нет префикса пользователя
	}
	for _, c := range no {
		if dbLooksRelated(c[0], c[1], c[2]) {
			t.Errorf("база %s ошибочно связалась с сайтом %s", c[0], c[2])
		}
	}
}

// Полный разбор каталога панели: ровно то, что происходит на сервере
// с HestiaCP при нажатии «Обследовать».
func TestHestiaSites(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "data", "users", "admin")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	web := strings.Join([]string{
		`DOMAIN='shop.ru' IP='203.0.113.10' ALIAS='www.shop.ru' SSL='yes' BACKEND_TEMPLATE='PHP-8_2'`,
		`DOMAIN='blog.ru' IP='203.0.113.10' ALIAS='' SSL='no' BACKEND_TEMPLATE='PHP-7_4'`,
		``,
	}, "\n")
	if err := os.WriteFile(filepath.Join(userDir, "web.conf"), []byte(web), 0o644); err != nil {
		t.Fatal(err)
	}
	db := strings.Join([]string{
		`DB='admin_shop' DBUSER='admin_shop' HOST='localhost' TYPE='mysql'`,
		`DB='admin_blog' DBUSER='admin_blog' HOST='localhost' TYPE='mysql'`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(userDir, "db.conf"), []byte(db), 0o644); err != nil {
		t.Fatal(err)
	}

	sites, owners := hestiaSites(context.Background(), root)
	if len(sites) != 2 {
		t.Fatalf("найдено сайтов: %d", len(sites))
	}
	byName := map[string]Site{}
	for _, s := range sites {
		byName[s.Name] = s
	}

	shop := byName["shop.ru"]
	if shop.User != "admin" || shop.PHP != "8.2" || !shop.SSL {
		t.Fatalf("shop.ru разобран неверно: %+v", shop)
	}
	if shop.Root != "/home/admin/web/shop.ru/public_html" {
		t.Fatalf("корень сайта: %s", shop.Root)
	}
	if len(shop.Aliases) != 1 || shop.Aliases[0] != "www.shop.ru" {
		t.Fatalf("алиасы: %v", shop.Aliases)
	}
	if len(shop.Databases) != 1 || shop.Databases[0] != "admin_shop" {
		t.Fatalf("к сайту привязаны базы: %v", shop.Databases)
	}
	if byName["blog.ru"].SSL {
		t.Fatal("SSL='no' прочитан как включённый")
	}
	if owners["admin_shop"][0] != "admin" {
		t.Fatalf("владелец базы: %v", owners["admin_shop"])
	}
}

// Из карты сервера должен получаться план, который сразу можно запускать.
func TestSuggest(t *testing.T) {
	rep := &Report{
		Hostname: "web-1",
		OS:       "Ubuntu 22.04",
		Panel:    &Panel{Kind: "hestia", Version: "1.8.11", Path: "/usr/local/hestia"},
		Sites: []Site{
			{Name: "shop.ru", User: "admin", Root: "/home/admin/web/shop.ru/public_html",
				Source: "hestia", Size: 4 << 30},
		},
		MySQL: &DBServer{Reachable: true, Databases: []Database{
			{Name: "admin_shop", Size: 340 << 20},
		}},
		Docker:  &Docker{Version: "24.0", Volumes: []Volume{{Name: "redis-data", Size: 1 << 20}}},
		Configs: []string{"/etc/nginx", "/etc/php"},
	}

	p := Suggest(rep)
	if err := p.Validate(); err != nil {
		t.Fatalf("предложенный план не проходит проверку: %v", err)
	}

	kinds := map[plan.Kind]plan.Module{}
	for _, m := range p.Modules {
		kinds[m.Kind] = m
	}
	for _, want := range []plan.Kind{plan.KindFiles, plan.KindMySQL, plan.KindDocker,
		plan.KindConfigs, plan.KindHestia} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("в плане нет модуля %s", want)
		}
	}

	// Для сайта под панелью берём весь его каталог, а не только public_html:
	// рядом лежат private, tmp и логи домена.
	files := kinds[plan.KindFiles]
	if len(files.Paths) != 1 || files.Paths[0] != "/home/admin/web/shop.ru" {
		t.Fatalf("путь сайта: %v", files.Paths)
	}

	// Образы docker по умолчанию не сохраняются, контейнеры не останавливаются:
	// и то и другое стоит дорого и должно включаться осознанно.
	d := kinds[plan.KindDocker]
	if d.SaveImages || d.StopForDump {
		t.Fatalf("опасные умолчания у docker: %+v", d)
	}

	if got := rep.EstimatedSize(); got != (4<<30)+(340<<20)+(1<<20) {
		t.Fatalf("оценка объёма: %d", got)
	}
	if !strings.Contains(rep.Summary(), "hestia") {
		t.Fatalf("сводка не упоминает панель: %s", rep.Summary())
	}
}

func TestParseHumanSize(t *testing.T) {
	cases := map[string]int64{
		"1.5GB": 1610612736, "100MB": 104857600, "512kB": 524288,
		"0B": 0, "": 0, "мусор": 0, "2TB": 2199023255552,
	}
	for in, want := range cases {
		if got := parseHumanSize(in); got != want {
			t.Errorf("parseHumanSize(%q) = %d, ожидалось %d", in, got, want)
		}
	}
}
