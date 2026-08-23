package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
)

// pgCommand запускает утилиту PostgreSQL от имени системного пользователя
// postgres, когда мы root: иначе peer-аутентификация отклонит соединение.
func pgCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if os.Geteuid() != 0 {
		return exec.CommandContext(ctx, bin, args...)
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, bin)
	for _, a := range args {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'"'"'`)+"'")
	}
	return exec.CommandContext(ctx, "su", "-s", "/bin/sh", "postgres", "-c", strings.Join(quoted, " "))
}

// --- Восстановление на удалённый сервер -----------------------------------

// protoTarget отправляет узлы агенту по SSH.
//
// Десктоп при этом остаётся единственным местом, где есть ключи от
// репозитория: агент получает уже расшифрованные данные ровно того
// снимка, который выбрал человек, и не может дотянуться до остальных.
type protoTarget struct {
	w   *proto.Writer
	buf []byte
}

func NewProto(out io.Writer) Target {
	return &protoTarget{w: proto.NewWriter(out), buf: make([]byte, proto.MaxDataFrame)}
}

func (t *protoTarget) Node(n *repo.Node, content io.Reader) error {
	if err := t.w.Node(n); err != nil {
		return err
	}
	if content != nil {
		if _, err := t.w.CopyStream(content, t.buf, nil); err != nil {
			return err
		}
	}
	return t.w.NodeEnd()
}

func (t *protoTarget) Finish() error {
	if err := t.w.JSON(proto.FrameDone, proto.Done{}); err != nil {
		return err
	}
	return t.w.Flush()
}

// Apply принимает поток восстановления на стороне агента и раскладывает
// его через Target.
func Apply(ctx context.Context, in io.Reader, t Target) error {
	rd := proto.NewReader(in)
	var (
		cur  *repo.Node
		pipe *io.PipeWriter
		done = make(chan error, 1)
	)
	// Досрочный выход не должен оставлять горутину, вечно ждущую данных
	// из трубы, которую больше никто не наполнит. В долгоживущем процессе
	// (веб-сервер) такие горутины копятся молча.
	defer func() {
		if pipe != nil {
			pipe.CloseWithError(errors.New("autobak: поток восстановления прерван"))
			<-done
		}
	}()

	// Содержимое файла отдаётся Target потоком, а кадры приходят порциями:
	// io.Pipe соединяет одно с другим, не собирая файл целиком в памяти.
	finish := func() error {
		if cur == nil {
			return nil
		}
		if pipe != nil {
			pipe.Close()
			if err := <-done; err != nil {
				return fmt.Errorf("%s: %w", cur.Path, err)
			}
			pipe = nil
		}
		cur = nil
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ft, payload, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return finish()
			}
			return err
		}
		switch ft {
		case proto.FrameNode:
			if err := finish(); err != nil {
				return err
			}
			n, err := proto.DecodeJSON[repo.Node](payload)
			if err != nil {
				return err
			}
			cur = &n
			if n.Type == repo.NodeFile {
				pr, pw := io.Pipe()
				pipe = pw
				go func(node repo.Node) {
					err := t.Node(&node, pr)
					// Читатель обязан досушить трубу, иначе пишущая сторона
					// заблокируется навсегда на следующем кадре данных.
					io.Copy(io.Discard, pr)
					pr.CloseWithError(err)
					done <- err
				}(n)
			} else if err := t.Node(&n, nil); err != nil {
				return fmt.Errorf("%s: %w", n.Path, err)
			}

		case proto.FrameData:
			if pipe == nil {
				return errors.New("autobak: данные пришли раньше описания файла")
			}
			if _, err := pipe.Write(payload); err != nil {
				return err
			}

		case proto.FrameNodeEnd:
			if err := finish(); err != nil {
				return err
			}

		case proto.FrameError:
			e, _ := proto.DecodeJSON[proto.ErrorMsg](payload)
			return fmt.Errorf("autobak: восстановление прервано: %s", e.Msg)

		case proto.FrameDone:
			if err := finish(); err != nil {
				return err
			}
			return t.Finish()
		}
	}
}
