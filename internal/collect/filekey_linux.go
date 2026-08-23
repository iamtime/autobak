//go:build linux

package collect

import (
	"os"
	"syscall"
)

// fileKey однозначно опознаёт объект файловой системы.
//
// Пара «устройство + инод» - единственный надёжный способ понять, что
// две ссылки ведут в одно место. Сравнение путей здесь не работает:
// /a/link и /srv/real - разные строки, указывающие на один каталог.
type fileKey struct{ dev, ino uint64 }

func keyOf(fi os.FileInfo) (fileKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{dev: uint64(st.Dev), ino: st.Ino}, true
}
