//go:build windows

package secretstore

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DPAPI - штатный механизм Windows для хранения секретов приложения.
//
// Ключ шифрования выводится из учётных данных пользователя и хранится
// системой. Практический смысл: файл secrets.dat, скопированный на другой
// компьютер или прочитанный другой учётной записью, бесполезен. Своя
// реализация «мастер-пароля» такого свойства не даёт - пользователь
// выберет тот же пароль, что и везде.
var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procCryptProtectData = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = crypt32.NewProc("CryptUnprotectData")
	procLocalFree        = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func (b dataBlob) bytes() []byte {
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

// entropy подмешивается в ключ: секреты autobak не расшифрует другое
// приложение того же пользователя, даже зная путь к файлу.
var entropy = []byte("autobak/v1 secretstore")

func protect(plain []byte) ([]byte, error) {
	in, ent := newBlob(plain), newBlob(entropy)
	var out dataBlob
	// CRYPTPROTECT_UI_FORBIDDEN: приложение может работать в фоне, и
	// системный диалог там означал бы намертво зависшее расписание.
	const uiForbidden = 0x1
	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, uintptr(unsafe.Pointer(&ent)),
		0, 0, uiForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("autobak: DPAPI не смог зашифровать: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func unprotect(sealed []byte) ([]byte, error) {
	in, ent := newBlob(sealed), newBlob(entropy)
	var out dataBlob
	const uiForbidden = 0x1
	r, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)), 0, uintptr(unsafe.Pointer(&ent)),
		0, 0, uiForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("autobak: DPAPI не смог расшифровать (файл от другой учётной записи?): %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}
