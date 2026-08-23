//go:build !windows

package secretstore

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iamtime/autobak/internal/repo"
)

// Вне Windows системного хранилища такого же уровня нет, поэтому ключ
// лежит рядом, в файле с правами 0600.
//
// Это честно слабее DPAPI: тот, кто прочитал домашний каталог, получит и
// ключ. Но альтернатива - хранить секреты открытым текстом - хуже, а
// требовать мастер-пароль при каждом запуске фонового расписания
// бессмысленно: его пришлось бы где-то сохранить, и мы вернулись бы сюда же.
func keyPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "autobak", "secret.key")
}

func localKey() (*repo.MasterKey, error) {
	path := keyPath()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) != 32 {
			// Файл есть, но он не тот. Молча перезаписать - значит навсегда
			// потерять все уже сохранённые секреты. Лучше громкая ошибка:
			// человек разберётся, что это за файл, а не обнаружит пропажу
			// паролей позже.
			return nil, fmt.Errorf("autobak: файл ключа %s повреждён (%d байт вместо 32) - "+
				"проверьте его, прежде чем удалять: с ним связаны сохранённые пароли", path, len(raw))
		}
		return repo.LoadMasterKey(raw)
	case !os.IsNotExist(err):
		// Не «файла нет», а ошибка доступа/ввода-вывода. Генерировать новый
		// ключ поверх временно недоступного - та же потеря секретов.
		return nil, fmt.Errorf("autobak: не прочитать ключ хранилища секретов %s: %w", path, err)
	}

	// Файла действительно нет - создаём. O_EXCL против гонки: если два
	// процесса стартуют одновременно на первом запуске, второй не затрёт
	// ключ первого, а перечитает его.
	newRaw := make([]byte, 32)
	if _, err := rand.Read(newRaw); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Кто-то успел создать ключ между нашими проверкой и записью.
			// Перечитываем его, а не подсовываем свой.
			if raw, rerr := os.ReadFile(path); rerr == nil && len(raw) == 32 {
				return repo.LoadMasterKey(raw)
			}
		}
		return nil, fmt.Errorf("autobak: не создать ключ хранилища секретов: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(newRaw); err != nil {
		return nil, fmt.Errorf("autobak: не записать ключ хранилища секретов: %w", err)
	}
	return repo.LoadMasterKey(newRaw)
}

func protect(plain []byte) ([]byte, error) {
	k, err := localKey()
	if err != nil {
		return nil, err
	}
	return k.SealMeta("secretstore", plain)
}

func unprotect(sealed []byte) ([]byte, error) {
	k, err := localKey()
	if err != nil {
		return nil, err
	}
	return k.OpenMeta("secretstore", sealed)
}
