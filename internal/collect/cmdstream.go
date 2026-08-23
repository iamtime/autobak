package collect

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/iamtime/autobak/internal/repo"
)

// streamCommand запускает утилиту и отдаёт её stdout в Sink как содержимое
// файла.
//
// Ничего не буферизуется: дамп базы на 200 ГБ проходит через тот же поток,
// не требуя ни временного файла на сервере (где место могло кончиться), ни
// памяти под себя. Именно поэтому agent не делает «сначала дамп в /tmp,
// потом заливку» - самый частый способ уронить продакшн бэкапом.
func streamCommand(ctx context.Context, s Sink, n *repo.Node, cmd *exec.Cmd) error {
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("autobak: не запустить %s: %w", cmd.Path, err)
	}

	n.MTime = time.Now().UnixNano()
	sinkErr := s.File(n, &countingReader{r: stdout, path: n.Path, s: s})

	// Wait обязателен даже при ошибке Sink: иначе процесс останется висеть,
	// а mysqldump держит транзакцию и блокирует базу.
	waitErr := cmd.Wait()
	if sinkErr != nil {
		return sinkErr
	}
	if waitErr != nil {
		if tail := stderrTail(errBuf.Bytes(), 2000); tail != "" {
			return fmt.Errorf("%w: %s", waitErr, tail)
		}
		return waitErr
	}
	if tail := stderrTail(errBuf.Bytes(), 500); tail != "" {
		// Предупреждения на stderr при нулевом коде возврата - обычное дело
		// (устаревшие опции, недоступные таблицы), но потерять их нельзя.
		s.Logf("warn", "%s: %s", n.Path, tail)
	}
	return nil
}

// runCapture выполняет короткую команду и возвращает её вывод.
func runCapture(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w: %s", name, err, stderrTail(errBuf.Bytes(), 500))
	}
	return out.Bytes(), nil
}
