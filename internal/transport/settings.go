package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Settings — настройки агента. Читаются из settings.json в папке агента.
// Если файла нет, используются значения по умолчанию. Ничего здесь не привязано
// к конкретной машине: пути и порты можно переопределить в файле.
type Settings struct {
	SyncthingPath  string `json:"syncthingPath"`  // пусто = искать в PATH и в папке winget
	TailscalePath  string `json:"tailscalePath"`  // пусто = искать в PATH и в Program Files
	GUIPort        int    `json:"guiPort"`        // порт REST/GUI Syncthing, слушает только 127.0.0.1
	SyncPort       int    `json:"syncPort"`       // порт синхронизации, слушает только IP Tailscale
	TestFolderID   string `json:"testFolderId"`   // идентификатор тестовой папки в Syncthing
	TestFolderPath string `json:"testFolderPath"` // где лежит тестовая папка на этой машине

	baseDir string // папка агента, не сериализуется
}

// DefaultBaseDir возвращает папку агента. Для тестов и второго экземпляра на
// той же машине её можно переопределить переменной окружения ENVSYNC_HOME.
func DefaultBaseDir() (string, error) {
	if v := os.Getenv("ENVSYNC_HOME"); v != "" {
		return v, nil
	}
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", errors.New("не задана переменная LOCALAPPDATA и нет ENVSYNC_HOME")
	}
	return filepath.Join(local, "EnvSync"), nil
}

// LoadSettings читает baseDir/settings.json поверх значений по умолчанию.
func LoadSettings(baseDir string) (Settings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Settings{}, fmt.Errorf("не удалось определить домашнюю папку: %w", err)
	}
	s := Settings{
		GUIPort:        18384, // не 8384, чтобы не конфликтовать с ручной установкой Syncthing
		SyncPort:       22000,
		TestFolderID:   "envsync-test",
		TestFolderPath: filepath.Join(home, "EnvSyncTest"),
		baseDir:        baseDir,
	}
	data, err := os.ReadFile(filepath.Join(baseDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return Settings{}, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("settings.json: %w", err)
	}
	s.baseDir = baseDir
	return s, nil
}

// BaseDir — папка агента.
func (s Settings) BaseDir() string { return s.baseDir }

// SyncthingHome — отдельная папка Syncthing, чтобы не мешать ручной установке.
func (s Settings) SyncthingHome() string { return filepath.Join(s.baseDir, "syncthing") }

// ConfigXMLPath — путь к config.xml Syncthing внутри его папки.
func (s Settings) ConfigXMLPath() string { return filepath.Join(s.SyncthingHome(), "config.xml") }

// findBinary ищет исполняемый файл: явный путь, затем PATH, затем шаблоны-запасные пути.
// Запасные пути нужны, потому что сразу после установки через winget PATH в уже открытых
// окнах ещё старый.
func findBinary(explicit, name string, fallbackGlobs ...string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%s: указанный путь недоступен: %w", name, err)
		}
		return explicit, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, g := range fallbackGlobs {
		if matches, _ := filepath.Glob(g); len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("%s не найден: установите его или укажите путь в settings.json", name)
}

// FindSyncthing возвращает путь к syncthing.exe.
func FindSyncthing(s Settings) (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	return findBinary(s.SyncthingPath, "syncthing",
		filepath.Join(local, "Microsoft", "WinGet", "Packages", "Syncthing.Syncthing_*", "*", "syncthing.exe"))
}

// FindTailscale возвращает путь к tailscale.exe.
func FindTailscale(s Settings) (string, error) {
	pf := os.Getenv("ProgramFiles")
	return findBinary(s.TailscalePath, "tailscale", filepath.Join(pf, "Tailscale", "tailscale.exe"))
}
