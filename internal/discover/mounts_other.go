//go:build !linux

package discover

// Обнаружение рассчитано на Linux-сервер. На других платформах пакет
// собирается только ради типов, которые использует десктоп.
func detectMounts() []Mount { return nil }
