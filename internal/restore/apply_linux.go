//go:build linux

package restore

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/iamtime/autobak/internal/repo"
	"golang.org/x/sys/unix"
)

// idResolver превращает имена пользователей и групп из снимка в числовые
// id этого сервера.
//
// Именно так, а не по сохранённым числам: на новом сервере пользователь
// www-data вполне может иметь другой uid, и восстановление по числам
// раздало бы файлы сайта постороннему. Если имени на сервере нет,
// откатываемся к сохранённому числу - это лучше, чем ничего.
type idResolver struct {
	once   sync.Once
	users  map[string]int
	groups map[string]int
}

func newIDResolver() *idResolver { return &idResolver{} }

func (r *idResolver) load() {
	r.users = parseNameIndex("/etc/passwd")
	r.groups = parseNameIndex("/etc/group")
}

func parseNameIndex(path string) map[string]int {
	out := map[string]int{}
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
		if id, err := strconv.Atoi(parts[2]); err == nil {
			out[parts[0]] = id
		}
	}
	return out
}

func (r *idResolver) uid(name string, fallback uint32) int {
	r.once.Do(r.load)
	if id, ok := r.users[name]; ok && name != "" {
		return id
	}
	return int(fallback)
}

func (r *idResolver) gid(name string, fallback uint32) int {
	r.once.Do(r.load)
	if id, ok := r.groups[name]; ok && name != "" {
		return id
	}
	return int(fallback)
}

func applyOwner(dst string, n *repo.Node, res *idResolver, enabled bool) error {
	if !enabled || os.Geteuid() != 0 {
		return nil
	}
	// Lchown, а не Chown: для символической ссылки владельца надо менять
	// у неё самой, а не у того, куда она указывает.
	return os.Lchown(dst, res.uid(n.User, n.UID), res.gid(n.Group, n.GID))
}

// applyModeBits возвращает setuid, setgid и sticky.
//
// Отдельно от Chmod при записи, потому что переименование и смена
// владельца сбрасывают эти биты - их надо ставить последними.
func applyModeBits(dst string, n *repo.Node, restoreOwner bool) {
	extra := n.Mode & 0o7000
	if extra == 0 {
		return
	}
	// setuid/setgid ставим только при восстановлении на сервер (RestoreOwner).
	// В безопасном локальном режиме они бессмысленны, а из враждебного
	// снимка дали бы setuid-root бинарь. sticky-бит безвреден - оставляем.
	if !restoreOwner {
		extra &^= 0o6000
	}
	mode := os.FileMode(n.Mode & 0o777)
	if extra&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if extra&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if extra&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	_ = os.Chmod(dst, mode)
}

func applyXattrs(dst string, n *repo.Node) {
	for k, v := range n.XAttrs {
		// Ошибки намеренно игнорируются: часть атрибутов (security.*,
		// selinux) не ставится без соответствующих прав, а некоторые
		// файловые системы их вовсе не поддерживают. Это не повод
		// прерывать восстановление.
		_ = unix.Lsetxattr(dst, k, []byte(v), 0)
	}
}

func makeSpecial(dst string, n *repo.Node, restoreOwner bool) error {
	// Узлы устройств создаём только при явном восстановлении на сервер:
	// mknod из снимка - это доступ к сырому диску или /dev/mem, и в
	// локальный безопасный каталог такому не место.
	if !restoreOwner || os.Geteuid() != 0 {
		return os.ErrPermission
	}
	mode := n.Mode & 0o777
	switch n.Type {
	case repo.NodeFIFO:
		return unix.Mkfifo(dst, mode)
	case repo.NodeDevice:
		return unix.Mknod(dst, mode|unix.S_IFBLK, int(n.Dev))
	case repo.NodeChar:
		return unix.Mknod(dst, mode|unix.S_IFCHR, int(n.Dev))
	case repo.NodeSocket:
		// Сокет создаёт своё приложение при старте. Воссоздавать его
		// файлом бессмысленно и вредно: демон потом не сможет привязаться.
		return nil
	}
	return nil
}
