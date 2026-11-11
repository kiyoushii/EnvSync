package appconfig

import (
	"context"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/proc"
)

// DefaultApps — приложения и файлы по умолчанию. Это белый список: что здесь не названо, не берётся.
//
// Состав выбран так, чтобы в него не попадали секреты и данные машины:
//   - git: только .gitconfig и общий gitignore, без ключей и менеджера учётных данных;
//   - VS Code: настройки, сочетания клавиш и сниппеты; расширения и кэш не берём;
//   - Windows Terminal: settings.json;
//   - Claude: инструкции и расширения (CLAUDE.md, commands, agents, skills). settings.json, учётные данные,
//     история и сессии не берутся, потому что там живут токены и пути, привязанные к машине.
func DefaultApps() []App {
	return []App{
		{ID: "git", Files: []FileSpec{
			{Root: Home, Path: ".gitconfig"},
			{Root: Home, Path: ".gitignore_global"},
		}},
		{ID: "vscode", Files: []FileSpec{
			{Root: AppData, Path: "Code/User/settings.json"},
			{Root: AppData, Path: "Code/User/keybindings.json"},
			{Root: AppData, Path: "Code/User/snippets", Dir: true},
		}},
		{ID: "windows-terminal", Files: []FileSpec{
			{Root: LocalAppData, Path: "Packages/Microsoft.WindowsTerminal_8wekyb3d8bbwe/LocalState/settings.json"},
		}},
		{ID: "claude", Files: []FileSpec{
			{Root: Home, Path: ".claude/CLAUDE.md"},
			{Root: Home, Path: ".claude/commands", Dir: true},
			{Root: Home, Path: ".claude/agents", Dir: true},
			// skills/synced Claude ведёт сам через аккаунт (в путях идентификаторы аккаунта), дублировать не нужно.
			{Root: Home, Path: ".claude/skills", Dir: true, SkipDirs: []string{"synced"}},
		}},
	}
}

// RunningProcesses спрашивает у Windows список запущенных процессов (tasklist) и возвращает их имена в нижнем регистре.
func RunningProcesses() (map[string]bool, error) {
	out, err := proc.Command(context.Background(), "tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		if name, _, ok := strings.Cut(line[1:], `"`); ok {
			names[strings.ToLower(name)] = true
		}
	}
	return names, nil
}
