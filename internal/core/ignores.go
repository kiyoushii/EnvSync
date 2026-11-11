package core

import "strings"

// DefaultIgnores — что не синкаем по умолчанию, если манифест не задаёт свой список.
// Шаблоны в синтаксисе Syncthing (.stignore): без слэша шаблон срабатывает на любой глубине;
// (?i) делает сравнение регистронезависимым, как в Windows. Список совпадает с исключениями
// в ARCHITECTURE.md (раздел 4) и с папками, которые scripts/measure.ps1 не считает.
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

// CountPatterns считает шаблоны без комментариев и пустых строк (для dry-run).
func CountPatterns(lines []string) int {
	n := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "//") {
			n++
		}
	}
	return n
}
