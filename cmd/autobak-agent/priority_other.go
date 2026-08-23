//go:build !linux

package main

import (
	"fmt"
	"os"

	"github.com/iamtime/autobak/internal/plan"
)

// Приоритеты процесса настраиваются только под Linux - там и работает
// агент. Сборка под другие платформы нужна лишь для запуска тестов.
func applyPriority(p *plan.Plan) {}

// checkPrivateFile под Windows проверяет лишь существование файла: модель
// прав здесь другая, и имитировать проверку chmod было бы враньём.
func checkPrivateFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("autobak: %s недоступен: %w", path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("autobak: %s - каталог, ожидался файл", path)
	}
	return nil
}
