//go:build linux

package discover

import (
	"bufio"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// detectMounts показывает, сколько места на разделах.
//
// Нужно не для красоты: перед восстановлением интерфейс обязан проверить,
// что данные вообще влезут, а перед бэкапом - предупредить, что на разделе
// с базами почти нет места для временных файлов.
func detectMounts() []Mount {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	// Виртуальные и сетевые файловые системы к делу не относятся и только
	// засоряют список.
	skipFS := []string{
		"proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "cgroup", "cgroup2",
		"securityfs", "pstore", "bpf", "debugfs", "tracefs", "configfs",
		"fusectl", "mqueue", "hugetlbfs", "autofs", "squashfs", "overlay",
		"binfmt_misc", "nsfs", "ramfs", "efivarfs",
	}

	var out []Mount
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || slices.Contains(skipFS, fields[2]) {
			continue
		}
		// В /proc/mounts пробелы и табуляции экранированы восьмеричными кодами.
		mp := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(fields[1])
		if seen[mp] {
			continue
		}
		var st unix.Statfs_t
		if unix.Statfs(mp, &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[mp] = true
		out = append(out, Mount{
			Path:  mp,
			Total: int64(st.Blocks) * int64(st.Bsize),
			// Bavail, а не Bfree: часть блоков зарезервирована под root,
			// и обещать их пользователю было бы враньём.
			Free: int64(st.Bavail) * int64(st.Bsize),
		})
	}
	return out
}
