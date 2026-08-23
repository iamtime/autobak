//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/iamtime/autobak/internal/plan"
	"golang.org/x/sys/unix"
)

// applyPriority опускает приоритет агента по процессору и диску.
//
// Бэкап не должен быть заметен посетителям сайта. Особенно важен ionice:
// чтение сотен тысяч файлов насыщает очередь диска, и без понижения
// приоритета ввода-вывода время ответа сайта вырастает в разы, даже когда
// процессор простаивает.
func applyPriority(p *plan.Plan) {
	if p.Nice > 0 {
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, p.Nice); err != nil {
			warnPriority("nice", err)
		}
	}
	if p.IONice > 0 {
		// Класс 2 - best-effort, данные 0..7, где 7 - самый низкий.
		const ioprioWhoProcess = 1
		const classBestEffort = 2
		level := min(p.IONice, 7)
		value := classBestEffort<<13 | level
		if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET,
			ioprioWhoProcess, 0, uintptr(value)); errno != 0 {
			warnPriority("ionice", errno)
		}
	}
}

func warnPriority(what string, err error) {
	// Не фатально: под непривилегированным пользователем понизить
	// приоритет иногда нельзя, но бэкап от этого не становится хуже.
	fmt.Fprintf(os.Stderr, "[warn] не удалось задать %s: %v\n", what, err)
}
