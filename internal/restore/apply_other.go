//go:build !linux

package restore

import (
	"errors"

	"github.com/iamtime/autobak/internal/repo"
)

// На Windows восстанавливать владельцев, xattr и устройства нечего и
// некуда: этот путь кода существует, чтобы десктоп мог скачать файлы
// из снимка к себе на диск.

type idResolver struct{}

func newIDResolver() *idResolver { return &idResolver{} }

func applyOwner(dst string, n *repo.Node, res *idResolver, enabled bool) error { return nil }

func applyModeBits(dst string, n *repo.Node, restoreOwner bool) {}

func applyXattrs(dst string, n *repo.Node) {}

func makeSpecial(dst string, n *repo.Node, restoreOwner bool) error {
	return errors.New("специальные файлы поддерживаются только на Linux")
}
