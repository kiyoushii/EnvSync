// Пакет proc запускает внешние программы (tailscale, syncthing, tasklist, winget, schtasks) так, чтобы у них
// не появлялось собственного окна консоли.
//
// Зачем: фоновый агент работает без консоли, и Windows открывает каждой запущенной им консольной утилите
// новое окно. При опросе Tailscale каждые 15 секунд это выглядело как мигающая командная строка на рабочем столе.
package proc

import (
	"context"
	"os/exec"
)

// Command — как exec.CommandContext, но без окна консоли у дочернего процесса.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	return cmd
}
