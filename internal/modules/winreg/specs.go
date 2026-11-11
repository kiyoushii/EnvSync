package winreg

// Белые списки ключей. Составлены по документации и сверены на реальной Windows 11 (сборка 26200):
// типы подтверждены чтением реестра, отсутствующие по умолчанию значения (например LaunchTo, TaskbarAl)
// передаются как «нет значения» и при применении удаляются.
//
// Добавить настройку = добавить строку сюда. Ключи вне этого списка агент не трогает.
const (
	keyPersonalize = `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`
	keyDWM         = `Software\Microsoft\Windows\DWM`
	keyAccent      = `Software\Microsoft\Windows\CurrentVersion\Explorer\Accent`
	keyAdvanced    = `Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`
	keySearch      = `Software\Microsoft\Windows\CurrentVersion\Search`
	keyIntl        = `Control Panel\International`
	keyKeyboard    = `Control Panel\Keyboard`
)

// AppearanceSpecs — тема, акцентный цвет, прозрачность.
var AppearanceSpecs = []Spec{
	{ID: "theme.apps_light", Key: keyPersonalize, Name: "AppsUseLightTheme", Type: "dword"},
	{ID: "theme.system_light", Key: keyPersonalize, Name: "SystemUsesLightTheme", Type: "dword"},
	{ID: "theme.transparency", Key: keyPersonalize, Name: "EnableTransparency", Type: "dword"},
	{ID: "theme.accent_on_surfaces", Key: keyPersonalize, Name: "ColorPrevalence", Type: "dword"},
	{ID: "theme.accent_color", Key: keyDWM, Name: "AccentColor", Type: "dword"},
	{ID: "theme.dwm_accent_on_borders", Key: keyDWM, Name: "ColorPrevalence", Type: "dword"},
	{ID: "theme.accent_menu", Key: keyAccent, Name: "AccentColorMenu", Type: "dword"},
	{ID: "theme.accent_start", Key: keyAccent, Name: "StartColorMenu", Type: "dword"},
	{ID: "theme.accent_palette", Key: keyAccent, Name: "AccentPalette", Type: "binary"},
}

// ExplorerSpecs — проводник и панель задач. Вступают в силу после перезапуска проводника.
var ExplorerSpecs = []Spec{
	{ID: "explorer.hide_file_ext", Key: keyAdvanced, Name: "HideFileExt", Type: "dword", Apply: Explorer},
	{ID: "explorer.show_hidden", Key: keyAdvanced, Name: "Hidden", Type: "dword", Apply: Explorer},
	{ID: "explorer.show_super_hidden", Key: keyAdvanced, Name: "ShowSuperHidden", Type: "dword", Apply: Explorer},
	{ID: "explorer.launch_to", Key: keyAdvanced, Name: "LaunchTo", Type: "dword", Apply: Explorer},
	{ID: "taskbar.align", Key: keyAdvanced, Name: "TaskbarAl", Type: "dword", Apply: Explorer},
	{ID: "taskbar.task_view", Key: keyAdvanced, Name: "ShowTaskViewButton", Type: "dword", Apply: Explorer},
	{ID: "taskbar.widgets", Key: keyAdvanced, Name: "TaskbarDa", Type: "dword", Apply: Explorer},
	{ID: "taskbar.chat", Key: keyAdvanced, Name: "TaskbarMn", Type: "dword", Apply: Explorer},
	{ID: "taskbar.copilot", Key: keyAdvanced, Name: "ShowCopilotButton", Type: "dword", Apply: Explorer},
	{ID: "taskbar.search_mode", Key: keySearch, Name: "SearchboxTaskbarMode", Type: "dword", Apply: Explorer},
}

// RegionalSpecs — форматы даты и времени, повтор клавиш. Применяются после выхода из системы.
// Раскладки и языки ввода сознательно не включены: они хранятся сложнее и ошибка делает ввод неработоспособным.
var RegionalSpecs = []Spec{
	{ID: "regional.short_date", Key: keyIntl, Name: "sShortDate", Type: "string", Apply: Logoff},
	{ID: "regional.long_date", Key: keyIntl, Name: "sLongDate", Type: "string", Apply: Logoff},
	{ID: "regional.short_time", Key: keyIntl, Name: "sShortTime", Type: "string", Apply: Logoff},
	{ID: "regional.time_format", Key: keyIntl, Name: "sTimeFormat", Type: "string", Apply: Logoff},
	{ID: "regional.first_day_of_week", Key: keyIntl, Name: "iFirstDayOfWeek", Type: "string", Apply: Logoff},
	{ID: "keyboard.delay", Key: keyKeyboard, Name: "KeyboardDelay", Type: "string", Apply: Logoff},
	{ID: "keyboard.speed", Key: keyKeyboard, Name: "KeyboardSpeed", Type: "string", Apply: Logoff},
}
