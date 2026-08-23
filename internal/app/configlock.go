package app

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Межпроцессная блокировка конфигурации.
//
// Внутри одного процесса запись сериализует cfgMu, но каталог настроек
// может делить несколько процессов: открытое окно, ночной `schedule run`
// из планировщика, веб-сервер. Без общей блокировки два одновременных
// `schedule run` построят одинаковую очередь и сделают двойные бэкапы, а
// параллельные записи конфига будут гонкой (атомарная запись через rename
// спасает файл от порчи, но не от того, что двое одновременно пишут).
//
// Реализация - lock-файл через O_CREATE|O_EXCL: работает одинаково на всех
// платформах без внешних зависимостей. Чужой lock-файл, который никто не
// обновлял дольше срока годности, считается брошенным (процесс упал) и
// перехватывается.
//
// Это не устраняет полностью «перезапись устаревшей копией из памяти»
// (окно, открытое всю ночь, при сохранении настройки затрёт Last, который
// за это время записал планировщик) - от этого спасает только перечитывание
// перед записью, а его в этот выпуск не заносим из-за риска. Рекомендация в
// документации: не держать несколько ПИШУЩИХ процессов на одном каталоге
// настроек одновременно.
const (
	configLockStale   = 30 * time.Second
	configLockWait    = 20 * time.Second
	configLockRefresh = 10 * time.Second
)

type configLock struct {
	path string
	f    *os.File
}

func lockConfig(dir string) (*configLock, error) {
	path := filepath.Join(dir, "config.lock")
	deadline := time.Now().Add(configLockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n%d\n", os.Getpid(), time.Now().UnixNano())
			return &configLock{path: path, f: f}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("autobak: не взять блокировку настроек: %w", err)
		}
		// Файл есть. Если он давно не менялся, владелец, скорее всего, умер -
		// перехватываем. Иначе ждём.
		if st, statErr := os.Stat(path); statErr == nil && time.Since(st.ModTime()) > configLockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("autobak: настройки заняты другим процессом (%s) дольше %s",
				path, configLockWait)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (l *configLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Close()
	_ = os.Remove(l.path)
}
