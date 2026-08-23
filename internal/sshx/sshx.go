// Package sshx запускает агента на сервере через системный ssh.
//
// Именно системный, а не встроенная библиотека: у администратора уже
// настроены ~/.ssh/config с алиасами и jump-хостами, ключи в агенте,
// known_hosts и, возможно, аппаратный токен. Своя реализация SSH всё это
// сломала бы ради сомнительной самодостаточности бинаря.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Target struct {
	// Alias - запись из ~/.ssh/config. Если задан, Host/Port/User и ключ
	// игнорируются: пусть решает конфигурация ssh.
	Alias string `json:"alias,omitempty"`

	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	User    string `json:"user,omitempty"`
	KeyPath string `json:"key_path,omitempty"`

	// AgentPath - где на сервере лежит бинарь агента.
	AgentPath string `json:"agent_path,omitempty"`
	// Sudo оборачивает вызов агента в sudo. Нужен, когда вход идёт под
	// непривилегированным пользователем, а читать надо чужие файлы.
	Sudo bool `json:"sudo,omitempty"`

	// ExtraArgs - дополнительные аргументы ssh (например, -J bastion).
	ExtraArgs []string `json:"extra_args,omitempty"`

	// Local запускает агента прямо здесь, без ssh. Нужен, когда autobak
	// стоит на самом сервере, и он же делает сквозную проверку всей
	// цепочки возможной без второй машины.
	Local bool `json:"local,omitempty"`
}

func (t Target) agentPath() string {
	if t.AgentPath == "" {
		return "/usr/local/bin/autobak-agent"
	}
	return t.AgentPath
}

func (t Target) destination() string {
	if t.Local {
		return "localhost"
	}
	if t.Alias != "" {
		return t.Alias
	}
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// Label - как сервер называется в сообщениях. Без секретов.
func (t Target) Label() string {
	if t.Local {
		return "этот компьютер"
	}
	d := t.destination()
	if t.Port != 0 && t.Port != 22 {
		d += ":" + strconv.Itoa(t.Port)
	}
	return d
}

func sshBinary() (string, error) {
	if p, err := exec.LookPath("ssh"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		// На Windows 10/11 клиент OpenSSH есть, но не всегда в PATH.
		fallback := `C:\Windows\System32\OpenSSH\ssh.exe`
		if _, err := os.Stat(fallback); err == nil {
			return fallback, nil
		}
		return "", errors.New(
			"autobak: не найден ssh. Установите «Клиент OpenSSH» в Параметрах → Приложения → Дополнительные компоненты")
	}
	return "", errors.New("autobak: не найден ssh - установите клиент OpenSSH")
}

func (t Target) sshArgs() []string {
	args := []string{
		"-o", "BatchMode=yes", // без интерактивных запросов пароля: расписание не должно зависать
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=6",
	}
	if t.Alias == "" {
		// Проверку хоста не ослабляем: подмена сервера означала бы, что
		// данные всех сайтов уедут не туда. Новый хост добавится сам,
		// изменившийся - остановит работу.
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
		if t.Port != 0 {
			args = append(args, "-p", strconv.Itoa(t.Port))
		}
		if t.KeyPath != "" {
			args = append(args, "-i", t.KeyPath, "-o", "IdentitiesOnly=yes")
		}
	}
	args = append(args, t.ExtraArgs...)
	// "--" завершает разбор опций ssh: без него адрес назначения вида
	// "-oProxyCommand=touch /tmp/x" был бы воспринят ssh как опция и
	// выполнил бы команду уже на этой машине. Адрес может задавать менее
	// доверенный пользователь (в веб-режиме - тот, кто добавляет сервер),
	// поэтому это не теоретический риск.
	return append(args, "--", t.destination())
}

// Agent собирает команду запуска агента на сервере.
//
// Аргументы склеиваются в одну строку: удалённый sshd передаёт команду
// в шелл целиком. Каждый аргумент экранируется - в них попадают пути и
// имена баз, то есть данные, которым нельзя доверять.
func (t Target) Agent(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if t.Local {
		return exec.CommandContext(ctx, t.agentPath(), args...), nil
	}
	bin, err := sshBinary()
	if err != nil {
		return nil, err
	}
	remote := t.agentPath()
	if t.Sudo {
		remote = "sudo -n " + remote
	}
	parts := []string{remote}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	full := append(t.sshArgs(), strings.Join(parts, " "))
	return exec.CommandContext(ctx, bin, full...), nil
}

// Raw выполняет произвольную команду на сервере.
//
// Используется только при установке агента, когда ключ ещё не ограничен
// через command= в authorized_keys. В обычной работе не применяется.
func (t Target) Raw(ctx context.Context, command string) (*exec.Cmd, error) {
	bin, err := sshBinary()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, append(t.sshArgs(), command)...), nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Result - итог короткой команды.
type Result struct {
	Stdout string
	Stderr string
}

// RunAgent выполняет команду агента и возвращает её вывод целиком.
// Для потоковых операций (export/import) используйте Agent напрямую.
func (t Target) RunAgent(ctx context.Context, timeout time.Duration, args ...string) (*Result, error) {
	return t.RunAgentInput(ctx, timeout, nil, args...)
}

// RunAgentInput - то же, но с данными на stdin (например, план для estimate).
func (t Target) RunAgentInput(ctx context.Context, timeout time.Duration, stdin io.Reader, args ...string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := t.Agent(ctx, args...)
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return &Result{Stdout: out.String(), Stderr: errBuf.String()},
			fmt.Errorf("autobak: %s: %w: %s", t.Label(), err, tail(errBuf.String(), 800))
	}
	return &Result{Stdout: out.String(), Stderr: errBuf.String()}, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// Version спрашивает версию установленного агента. Заодно это самая
// дешёвая проверка того, что связь есть и агент на месте.
func (t Target) Version(ctx context.Context) (string, error) {
	res, err := t.RunAgent(ctx, 30*time.Second, "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Install заливает бинарь агента на сервер.
//
// Требует обычного, не ограниченного доступа: ограничивать ключ через
// command= в authorized_keys имеет смысл уже после установки.
func (t Target) Install(ctx context.Context, binary io.Reader) error {
	if t.Local {
		return errors.New("autobak: локальному агенту установка не требуется")
	}
	remote := t.agentPath()
	sudo := ""
	if t.Sudo {
		sudo = "sudo -n "
	}
	// Запись во временный файл и atomic mv: работающий в этот момент
	// бэкап не должен получить наполовину записанный бинарь.
	script := fmt.Sprintf(
		"set -e; tmp=$(mktemp); cat > $tmp; chmod 0755 $tmp; %[1]smkdir -p %[2]s; %[1]smv $tmp %[3]s; %[3]s version",
		sudo, shellQuote(dirOf(remote)), shellQuote(remote))

	cmd, err := t.Raw(ctx, script)
	if err != nil {
		return err
	}
	cmd.Stdin = binary
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("autobak: установка агента на %s не удалась: %w: %s",
			t.Label(), err, tail(errBuf.String(), 800))
	}
	return nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/usr/local/bin"
}

// AuthorizedKeyLine - строка, которую нужно добавить в authorized_keys
// сервера, чтобы ключ давал доступ только к агенту и ничему больше.
//
// backupOnly дополнительно запрещает восстановление. Это стоит понимать:
// команда восстановления пишет произвольные файлы от root, поэтому ключ,
// которому она разрешена, равносилен root - им можно положить свой
// authorized_keys. Ключ только для бэкапов такой возможности не даёт,
// а на время восстановления строку меняют руками.
func AuthorizedKeyLine(publicKey, agentPath string, backupOnly bool, allowArgs []string) string {
	if agentPath == "" {
		agentPath = "/usr/local/bin/autobak-agent"
	}
	parts := []string{agentPath, "serve"}
	if backupOnly {
		parts = append(parts, "--backup-only")
	}
	// Ограничение доступа (--allow=..., --allow-db) выводится из плана и
	// вписывается сюда: так украденный ключ ограничен ровно теми
	// каталогами, которые и так бэкапились, а не всем сервером.
	parts = append(parts, allowArgs...)
	return fmt.Sprintf(
		`command="%s",restrict %s`,
		strings.Join(parts, " "), strings.TrimSpace(publicKey))
}
