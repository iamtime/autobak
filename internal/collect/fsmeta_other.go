//go:build !linux

package collect

import (
	"os"

	"github.com/iamtime/autobak/internal/repo"
)

// Заглушки для сборки не под Linux.
//
// Агент работает только на Linux, но десктоп собирается под Windows и
// использует тот же пакет: он умеет восстанавливать файлы к себе на диск
// и запускать тесты сборщиков. Владельцев и xattr в этом случае просто нет.

func fillSysMeta(n *repo.Node, path string, fi os.FileInfo, uc *userCache) {}

func deviceOf(fi os.FileInfo) uint64 { return 0 }

type userCache struct{}

func newUserCache() *userCache { return &userCache{} }

func (c *userCache) userName(uid uint32) string  { return "" }
func (c *userCache) groupName(gid uint32) string { return "" }
