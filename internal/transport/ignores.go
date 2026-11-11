package transport

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// DefaultIgnores — что не синкаем по умолчанию. Шаблоны в синтаксисе Syncthing (.stignore):
// без слэша шаблон срабатывает на любой глубине; (?i) делает сравнение регистронезависимым,
// как в Windows. Список совпадает с исключениями в ARCHITECTURE.md (раздел 4) и с папками,
// которые scripts/measure.ps1 не считает.
//
// `.git` не синкаем по решению этапа 0: параллельные правки ломают репозиторий,
// репозитории разводятся через git push/pull.
var DefaultIgnores = []string{
	"// Служебное: папка агента не должна синкаться сама в себя",
	"(?i).envsync",

	"// Репозитории (решение этапа 0)",
	"(?i).git",

	"// Кэши и сборка: восстанавливаются, весят много",
	"(?i)node_modules",
	"(?i)__pycache__",
	"(?i)*.pyc",
	"(?i)Cache",
	"(?i)Code Cache",
	"(?i)GPUCache",
	"(?i)Temp",

	"// Незавершённые загрузки и блокировки приложений",
	"(?i)*.crdownload",
	"(?i)*.part",
	"(?i)*.tmp",
	"(?i)~$*",

	"// Служебные папки и файлы Windows",
	"(?i)Thumbs.db",
	"(?i)$RECYCLE.BIN",
	"(?i)System Volume Information",
}

// IgnoresOrDefault возвращает настроенный в settings.json список или значения по умолчанию.
func (s Settings) IgnoresOrDefault() []string {
	if len(s.Ignores) > 0 {
		return s.Ignores
	}
	return DefaultIgnores
}

// countPatterns считает шаблоны без комментариев и пустых строк (для dry-run).
func countPatterns(lines []string) int {
	n := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "//") {
			n++
		}
	}
	return n
}

// SetIgnores записывает исключения папки через REST (заменяет весь список).
func (c *Client) SetIgnores(ctx context.Context, folderID string, lines []string) error {
	body := map[string]any{"ignore": lines}
	return c.do(ctx, http.MethodPost, "/rest/db/ignores?folder="+url.QueryEscape(folderID), body, nil)
}

// Ignores возвращает действующие исключения папки.
func (c *Client) Ignores(ctx context.Context, folderID string) ([]string, error) {
	var out struct {
		Ignore []string `json:"ignore"`
	}
	err := c.do(ctx, http.MethodGet, "/rest/db/ignores?folder="+url.QueryEscape(folderID), nil, &out)
	return out.Ignore, err
}
