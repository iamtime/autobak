package sshx

import (
	"context"
	"strings"
	"testing"
)

// Аргументы агента содержат пути и имена баз, то есть данные, которым
// нельзя доверять: они склеиваются в строку и попадают в шелл на сервере.
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":     "simple",
		"/etc/nginx": "/etc/nginx",
		"--plan=-":   "--plan=-",
		"":           "''",
		"с пробелом": "'с пробелом'",
		"a;rm -rf /": "'a;rm -rf /'",
		"$(whoami)":  "'$(whoami)'",
		"back`tick`": "'back`tick`'",
		"it's":       `'it'"'"'s'`,
		"a|b":        "'a|b'",
		"a&&b":       "'a&&b'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// Собранная команда не должна давать возможности выполнить что-то помимо
// агента, какими бы ни были аргументы.
func TestAgentCommandIsQuoted(t *testing.T) {
	tgt := Target{Host: "example.com", User: "root", AgentPath: "/usr/local/bin/autobak-agent"}
	cmd, err := tgt.Agent(context.Background(), "export", "--plan", "/tmp/a b; rm -rf /")
	if err != nil {
		t.Skipf("ssh недоступен: %v", err)
	}
	remote := cmd.Args[len(cmd.Args)-1]
	if !strings.HasPrefix(remote, "/usr/local/bin/autobak-agent export --plan '") {
		t.Fatalf("команда собрана неверно: %q", remote)
	}
	if strings.Contains(remote, "; rm -rf /'") == false {
		t.Fatalf("опасный аргумент потерян или не экранирован: %q", remote)
	}
	// Точка с запятой обязана оказаться внутри кавычек, а не разделять команды.
	if idx := strings.Index(remote, ";"); idx >= 0 {
		before := remote[:idx]
		if strings.Count(before, "'")%2 == 0 {
			t.Fatalf("точка с запятой оказалась вне кавычек: %q", remote)
		}
	}
}

func TestSSHArgsHardening(t *testing.T) {
	tgt := Target{Host: "example.com", Port: 2222, User: "admin", KeyPath: "C:/k/id"}
	args := strings.Join(tgt.sshArgs(), " ")
	for _, must := range []string{
		"BatchMode=yes",                    // расписание не должно зависать на запросе пароля
		"StrictHostKeyChecking=accept-new", // подмена известного хоста остановит работу
		"IdentitiesOnly=yes",               // агент ssh не подсунет посторонний ключ
		"-p 2222", "admin@example.com",
	} {
		if !strings.Contains(args, must) {
			t.Errorf("в аргументах ssh нет %q: %s", must, args)
		}
	}
}

// С алиасом из ~/.ssh/config всё решает конфигурация ssh: подставлять
// свои -p и -i поверх неё значит ломать jump-хосты и нестандартные схемы.
func TestAliasIgnoresOwnOptions(t *testing.T) {
	args := strings.Join(Target{Alias: "prod-web", Port: 2222, KeyPath: "x"}.sshArgs(), " ")
	if strings.Contains(args, "2222") || strings.Contains(args, "-i ") {
		t.Fatalf("при использовании алиаса подставлены свои параметры: %s", args)
	}
	if !strings.HasSuffix(args, "prod-web") {
		t.Fatalf("алиас не передан ssh: %s", args)
	}
}

func TestAuthorizedKeyLine(t *testing.T) {
	line := AuthorizedKeyLine("ssh-ed25519 AAAAC3Nza user@pc", "", false, nil)
	if !strings.HasPrefix(line, `command="/usr/local/bin/autobak-agent serve",restrict `) {
		t.Fatalf("строка ключа не ограничивает вызов: %s", line)
	}

	// Ключ только для бэкапов: восстановление им запрещено, потому что
	// оно пишет произвольные файлы от root и равносильно root-доступу.
	only := AuthorizedKeyLine("ssh-ed25519 AAAAC3Nza user@pc", "", true, nil)
	if !strings.Contains(only, "serve --backup-only") {
		t.Fatalf("режим только для бэкапов не задан: %s", only)
	}
	if !strings.Contains(only, ",restrict ") {
		t.Fatalf("ограничение потеряно: %s", only)
	}
	if !strings.HasSuffix(line, "ssh-ed25519 AAAAC3Nza user@pc") {
		t.Fatalf("сам ключ потерялся: %s", line)
	}
	// restrict отключает проброс портов, агента, X11 и pty разом.
	// Перечислять их поимённо нельзя: в новых версиях OpenSSH список растёт.
	if !strings.Contains(line, ",restrict ") {
		t.Fatal("отсутствует restrict")
	}
}

func TestLocalTarget(t *testing.T) {
	tgt := Target{Local: true, AgentPath: "/opt/autobak-agent"}
	if tgt.Label() != "этот компьютер" {
		t.Fatalf("подпись локальной цели: %q", tgt.Label())
	}
	cmd, err := tgt.Agent(context.Background(), "version")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/opt/autobak-agent" || len(cmd.Args) != 2 || cmd.Args[1] != "version" {
		t.Fatalf("локальный вызов собран как %v", cmd.Args)
	}
	if err := tgt.Install(context.Background(), strings.NewReader("x")); err == nil {
		t.Fatal("установка локального агента должна отвергаться")
	}
}
