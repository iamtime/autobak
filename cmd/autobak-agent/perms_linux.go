//go:build linux

package main

import (
	"fmt"
	"os"
)

// checkPrivateFile отказывается работать с файлом, доступным не только
// владельцу.
//
// В конфигурации агента лежат ключи от хранилища, в файле ключа - сам
// master key. Файл с правами 644 означает, что любой пользователь сервера
// (включая процесс PHP взломанного сайта) читает бэкапы всего сервера.
// Поэтому здесь отказ, а не предупреждение: предупреждение в журнале
// systemd не прочитает никто.
func checkPrivateFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("autobak: %s недоступен: %w", path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("autobak: %s - каталог, ожидался файл", path)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf(
			"autobak: %s доступен посторонним (права %04o). Выполните: chmod 600 %s",
			path, mode, path)
	}
	return nil
}
