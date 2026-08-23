package gitmirror

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type git struct {
	dir string
	cfg *Config
}

// run выполняет git с настроенным доступом.
//
// Токен никогда не попадает ни в аргументы, ни в URL: и то и другое видно
// в списке процессов и остаётся в .git/config. Вместо этого пишется
// временный файл учётных данных с правами только для владельца, который
// удаляется сразу после команды.
func (g *git) run(ctx context.Context, args ...string) (string, error) {
	base := []string{
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
		"-c", "user.name=" + g.cfg.AuthorName,
		"-c", "user.email=" + g.cfg.AuthorEmail,
		// Подпись коммитов может быть включена глобально и потребует
		// ввода пароля от ключа - в фоновой задаче это зависание.
		"-c", "commit.gpgsign=false",
	}

	var cleanup func()
	if g.cfg.Token != "" {
		file, done, err := g.credentialsFile()
		if err != nil {
			return "", err
		}
		cleanup = done
		base = append(base,
			"-c", "credential.helper=",
			"-c", "credential.helper=store --file="+filepath.ToSlash(file))
	}
	if cleanup != nil {
		defer cleanup()
	}

	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // лучше внятная ошибка, чем зависший процесс
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	)
	if g.cfg.SSHKey != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+sshCommand(g.cfg.SSHKey))
	}

	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, redact(msg, g.cfg.Token))
	}
	return out.String(), nil
}

func sshCommand(key string) string {
	// IdentitiesOnly не даёт ssh-агенту подсунуть другой ключ, а
	// accept-new принимает только ранее неизвестные хосты: подмена
	// уже известного сервера по-прежнему остановит операцию.
	return fmt.Sprintf("ssh -i %q -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new", key)
}

// redact вычищает токен из сообщений об ошибках: они попадают в журнал
// и в интерфейс, и утечка через текст ошибки - вполне реальный путь.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

func (g *git) credentialsFile() (string, func(), error) {
	u, err := url.Parse(g.cfg.Remote)
	if err != nil {
		return "", nil, fmt.Errorf("autobak: некорректный адрес git-репозитория: %w", err)
	}
	user := g.cfg.User
	if user == "" {
		user = "git"
	}
	f, err := os.CreateTemp("", "autobak-git-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	done := func() { os.Remove(name) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		done()
		return "", nil, err
	}
	line := fmt.Sprintf("%s://%s:%s@%s\n",
		u.Scheme, url.QueryEscape(user), url.QueryEscape(g.cfg.Token), u.Host)
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		done()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		done()
		return "", nil, err
	}
	return name, done, nil
}

// ensureRepo создаёт или открывает рабочий клон и переключается на ветку.
func (g *git) ensureRepo(ctx context.Context) error {
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err != nil {
		if g.cfg.Remote != "" {
			// Клонируем, если репозиторий уже существует на той стороне;
			// если он пуст или недоступен - начинаем локально и запушим позже.
			if _, err := g.run(ctx, "clone", "--origin", "origin", g.cfg.Remote, "."); err != nil {
				g.cfg.logf("warn", "клонирование не удалось, начинаем локальную историю: "+err.Error())
				if _, err := g.run(ctx, "init"); err != nil {
					return err
				}
				if _, err := g.run(ctx, "remote", "add", "origin", g.cfg.Remote); err != nil {
					return err
				}
			}
		} else if _, err := g.run(ctx, "init"); err != nil {
			return err
		}
	}

	// Ветка: пробуем переключиться, иначе создаём.
	if _, err := g.run(ctx, "checkout", g.cfg.Branch); err != nil {
		if _, err := g.run(ctx, "checkout", "-b", g.cfg.Branch); err != nil {
			return err
		}
	}
	if g.cfg.Remote != "" {
		// Подтягиваем чужие изменения, чтобы push не отвергли. Ошибка не
		// фатальна: репозитория на той стороне может ещё не быть.
		if _, err := g.run(ctx, "pull", "--rebase", "--autostash", "origin", g.cfg.Branch); err != nil {
			g.cfg.logf("info", "нечего подтягивать из remote")
		}
	}
	return nil
}

func (g *git) hasChanges(ctx context.Context) (bool, error) {
	if _, err := g.run(ctx, "add", "-A", "--", g.cfg.Prefix); err != nil {
		return false, err
	}
	out, err := g.run(ctx, "status", "--porcelain", "--", g.cfg.Prefix)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (g *git) commit(ctx context.Context, message string) (string, error) {
	if _, err := g.run(ctx, "commit", "-m", message, "--", g.cfg.Prefix); err != nil {
		return "", err
	}
	sha, err := g.run(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(sha), err
}

func (g *git) push(ctx context.Context) error {
	_, err := g.run(ctx, "push", "-u", "origin", g.cfg.Branch)
	return err
}
