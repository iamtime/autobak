//go:build !linux

package collect

import "os"

type fileKey struct{ dev, ino uint64 }

// Вне Linux опознать каталог по иноду нечем, поэтому защиты от петель
// при переходе по ссылкам нет. Агент работает только на Linux; здесь
// код собирается ради тестов.
func keyOf(fi os.FileInfo) (fileKey, bool) { return fileKey{}, false }
