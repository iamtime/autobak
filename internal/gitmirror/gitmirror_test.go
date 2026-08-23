package gitmirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/engine"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

func TestLooksSecret(t *testing.T) {
	secret := []string{
		"/home/admin/web/site.ru/.env",
		"/etc/ssl/private/site.key",
		"/etc/letsencrypt/live/site.ru/privkey.pem",
		"/root/.my.cnf",
		"/usr/local/hestia/conf/mysql.conf",
		"/etc/ssh/ssh_host_rsa_key",
		"/etc/shadow",
	}
	for _, p := range secret {
		if !looksSecret(p) {
			t.Errorf("секрет не распознан: %s", p)
		}
	}
	ok := []string{
		"/etc/nginx/nginx.conf",
		"/etc/php/8.2/fpm/pool.d/www.conf",
		"/opt/stack/docker-compose.yml",
	}
	for _, p := range ok {
		if looksSecret(p) {
			t.Errorf("обычный конфиг принят за секрет: %s", p)
		}
	}
}

func TestKeepInSchema(t *testing.T) {
	drop := []string{
		"INSERT INTO `orders` VALUES (1),(2);",
		"  REPLACE INTO x VALUES (1);",
		"LOCK TABLES `orders` WRITE;",
		// Хеши паролей учёток и ролей: в git не должны попадать никогда.
		"GRANT ALL PRIVILEGES ON *.* TO 'admin'@'%' IDENTIFIED BY PASSWORD '*81F5E21E35407D884A6CD4A731AEBFB6AF209E1B';",
		"CREATE USER 'app'@'localhost' IDENTIFIED WITH mysql_native_password AS '*ABCDEF';",
		"CREATE ROLE app WITH LOGIN PASSWORD 'SCRAM-SHA-256$4096:abcd';",
		"ALTER ROLE app PASSWORD E'md5deadbeef';",
		"SET PASSWORD FOR 'x'@'localhost' = '*HASH';",
	}
	for _, l := range drop {
		if keepInSchema([]byte(l)) {
			t.Errorf("строка с данными не отброшена: %s", l)
		}
	}
	keep := []string{
		"CREATE TABLE `orders` (",
		"  `id` int NOT NULL AUTO_INCREMENT,",
		"  KEY `idx_created` (`created_at`)",
		// Колонка с именем password - это структура, а не секрет: остаётся.
		"  password text,",
		"  `password` varchar(255) NOT NULL,",
	}
	for _, l := range keep {
		if !keepInSchema([]byte(l)) {
			t.Errorf("строка структуры отброшена: %s", l)
		}
	}
}

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git не установлен - тест зеркала пропущен")
	}
}

func TestSyncCommitsConfigsAndSkipsSecrets(t *testing.T) {
	gitAvailable(t)
	ctx := context.Background()

	// Сервер: конфиг nginx, .env с паролем и дамп базы.
	src := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("nginx/nginx.conf", "server { listen 80; root /var/www; }\n")
	write("php/fpm/pool.d/www.conf", "pm.max_children = 20\n")
	write("site/.env", "DB_PASSWORD=оченьсекретно\n")

	be, err := backend.OpenLocal(t.TempDir(), backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := repo.Init(ctx, be, "пароль", "")
	if err != nil {
		t.Fatal(err)
	}
	p := plan.New("prod")
	p.Modules = []plan.Module{{
		Kind: plan.KindConfigs, Name: "Конфигурации", Enabled: true,
		Paths: []string{filepath.ToSlash(src)},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "prod", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	cfg := Config{
		Enabled: true, WorkDir: work, Branch: "main",
		Include: []string{filepath.ToSlash(src)},
		Log:     func(level, msg string) { t.Logf("[%s] %s", level, msg) },
	}
	rep, err := Sync(ctx, r, snap, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())

	if !rep.Changed || rep.Commit == "" {
		t.Fatal("первый запуск не создал коммит")
	}
	if len(rep.Secrets) != 1 || !strings.HasSuffix(rep.Secrets[0], ".env") {
		t.Fatalf("файл .env не был опознан как секрет: %v", rep.Secrets)
	}

	// Проверяем содержимое рабочего дерева git.
	found := map[string]bool{}
	err = filepath.Walk(work, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || strings.Contains(p, ".git"+string(os.PathSeparator)) {
			return nil
		}
		found[filepath.Base(p)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found["nginx.conf"] || !found["www.conf"] {
		t.Fatalf("конфигурации не попали в git: %v", found)
	}
	if found[".env"] {
		t.Fatal("секрет уехал в git-репозиторий")
	}
	if !found["README.md"] {
		t.Fatal("манифест снимка не создан")
	}

	// Повторный запуск без изменений не должен плодить пустые коммиты.
	rep2, err := Sync(ctx, r, snap, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Changed {
		t.Fatal("повторная синхронизация без изменений создала коммит")
	}

	// А правка конфига - должна.
	write("nginx/nginx.conf", "server { listen 443 ssl; root /var/www; }\n")
	snap2, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "prod", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	rep3, err := Sync(ctx, r, snap2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !rep3.Changed || rep3.Commit == rep.Commit {
		t.Fatal("изменение конфига не попало в историю")
	}

	// И это должно быть видно обычным git diff - ради чего всё и делалось.
	out, err := exec.Command("git", "-C", work, "log", "--oneline").Output()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(out)), "\n")); n != 2 {
		t.Fatalf("в истории %d коммитов вместо 2:\n%s", n, out)
	}
	diff, err := exec.Command("git", "-C", work, "diff", "HEAD~1", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "listen 443 ssl") {
		t.Fatalf("diff не показывает изменение:\n%s", diff)
	}
	t.Logf("diff между снимками:\n%s", firstLines(string(diff), 14))
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
