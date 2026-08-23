//go:build linux

package collect

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/iamtime/autobak/internal/repo"
	"golang.org/x/sys/unix"
)

// fillSysMeta переносит в узел то, что знает только ядро: владельца,
// группу, номер устройства и расширенные атрибуты.
//
// POSIX ACL живут в xattr system.posix_acl_access, поэтому отдельного кода
// для них не нужно - они сохраняются вместе с остальными атрибутами.
func fillSysMeta(n *repo.Node, path string, fi os.FileInfo, uc *userCache) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	n.UID, n.GID = st.Uid, st.Gid
	n.User = uc.userName(st.Uid)
	n.Group = uc.groupName(st.Gid)
	if n.Type == repo.NodeDevice || n.Type == repo.NodeChar {
		n.Dev = uint64(st.Rdev)
	}
	if x := readXattrs(path); len(x) > 0 {
		n.XAttrs = x
	}
}

// deviceOf возвращает номер устройства, на котором лежит файл.
// По нему обходчик определяет границы файловой системы и не проваливается
// в /proc, /sys и сетевые монтирования.
func deviceOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

func readXattrs(path string) map[string]string {
	sz, err := unix.Llistxattr(path, nil)
	if err != nil || sz <= 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(path, buf)
	if err != nil || sz <= 0 {
		return nil
	}
	out := map[string]string{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vsz, err := unix.Lgetxattr(path, name, nil)
		if err != nil || vsz < 0 || vsz > 64*1024 {
			continue
		}
		val := make([]byte, vsz)
		if _, err := unix.Lgetxattr(path, name, val); err != nil {
			continue
		}
		out[name] = string(val)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// userCache переводит uid и gid в имена.
//
// Имена нужны, потому что на новом сервере те же пользователи почти
// наверняка получат другие числовые id, и восстановление по числам
// раздало бы права посторонним учётным записям.
type userCache struct {
	once   sync.Once
	users  map[uint32]string
	groups map[uint32]string
}

func newUserCache() *userCache { return &userCache{} }

func (c *userCache) load() {
	c.users = parseIDFile("/etc/passwd")
	c.groups = parseIDFile("/etc/group")
}

func parseIDFile(path string) map[uint32]string {
	out := map[uint32]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 3 {
			continue
		}
		id, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			continue
		}
		out[uint32(id)] = parts[0]
	}
	return out
}

func (c *userCache) userName(uid uint32) string {
	c.once.Do(c.load)
	return c.users[uid]
}

func (c *userCache) groupName(gid uint32) string {
	c.once.Do(c.load)
	return c.groups[gid]
}
