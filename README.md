# EnvSync

Синхронизация среды Windows 11 между двумя машинами: файлы, настройки, оформление, программы.

Статус: этапы 0–7 и 9 реализованы (этап 8, VPS, опционален и ждёт сервера: [docs/stage-8.md](docs/stage-8.md)). Этапы 0–3 выполнены (план, сеть и сопряжение, файловый MVP, манифест и профили машин), этапы 4 (настройки и оформление), 5 (программы через winget), 6 (безопасность и откат) и 7 (веб-интерфейс и трей) реализованы и покрыты тестами.
Планы и результаты этапов — в [docs/](docs/), решения и причины — в [ARCHITECTURE.md](ARCHITECTURE.md).
Пример манифеста — [envsync.example.yaml](envsync.example.yaml).

## Замер среды (только чтение)

На каждой машине, в обычном PowerShell:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\measure.ps1
```

Скрипт ничего не меняет и не отправляет по сети. Отчёт сохраняется в `scripts\measure-<машина>.txt`.

## Сборка и тесты

Нужен Go и установленные Tailscale и Syncthing (`winget install --id GoLang.Go --exact` и т. д.).

```powershell
go build -o envsync.exe ./cmd/envsync
go test ./...
```

Интеграционные тесты запускают три настоящих Syncthing во временных папках на `127.0.0.1`
(сеть, брандмауэр и ваши данные не затрагиваются):

```powershell
$env:ENVSYNC_IT = "1"; go test ./internal/transport -run Integration -timeout 5m -v
```

## Команды агента (этап 1)

| Команда | Что делает |
|---|---|
| `envsync init` | создаёт ключи и конфиг Syncthing в `%LOCALAPPDATA%\EnvSync`, показывает Device ID |
| `envsync id` | Device ID и имя машины в Tailscale |
| `envsync run` | запускает Syncthing, слушающий только Tailscale; останавливает его при потере сети |
| `envsync status` | состояние Tailscale, Syncthing и связи со вторым устройством |
| `envsync audit` | проверяет, что конфиг Syncthing соответствует правилам безопасности |
| `envsync pair <имя-в-tailscale> <device-id>` | добавляет вторую машину (dry-run, затем подтверждение) |
| `envsync manifest check [--machine ИМЯ --home ПУТЬ]` | проверяет `envsync.yaml` и показывает, как он раскрывается на этой (или другой) машине; ничего не меняет |
| `envsync apply` | приводит папки в Syncthing в соответствие с манифестом (dry-run, затем подтверждение); только папки `test: true`, пока не включён `allowLiveData` |
| `envsync settings export` | снимает настройки этой машины (тема, обои, проводник, форматы, конфиги приложений) в папку состояний |
| `envsync settings plan [--only a,b]` | dry-run: что изменится на этой машине по состоянию другой; ничего не меняет |
| `envsync settings apply [--only a,b]` | применяет после подтверждения; перед этим делается снимок «до» |
| `envsync settings rollback [id]` / `runs` / `journal` | откат применения, список применений, журнал аудита |
| `envsync software scan` / `plan` | список установленных программ; dry-run по манифесту и предложения добавить новое в манифест |
| `envsync software apply [--uninstall]` / `rollback` | установка недостающего после подтверждения; удаление только явно перечисленного, с подтверждением каждого пакета |
| `envsync guard status` / `accept <папка>` / `reject <папка>` | предохранитель массового удаления: папка на паузе, решение человека |
| `envsync pause` / `resume` | «пауза всего» |
| `envsync run [--no-tray] [--no-ui]` | запускает Syncthing, предохранитель, веб-интерфейс (`127.0.0.1:18385`) и значок в трее |
| `envsync ui` / `checklist` | открыть веб-интерфейс запущенного агента; чек-лист ручной настройки новой машины |
| `envsync install` / `uninstall` | автозапуск агента при входе (задача планировщика, без администратора) |
| `envsync update` / `version` / `wait` | обновить Syncthing и Tailscale; версии; дождаться готовности агента |

## Новая машина одной командой

`scripts/bootstrap.ps1` ставит всё нужное, собирает агента, помогает войти в Tailscale, сопрягает машины и применяет манифест, программы и настройки
(`-DryRun` показывает шаги и ничего не меняет). Подробности и ограничения: [docs/stage-9.md](docs/stage-9.md).
| `envsync rollback [id]` | единая команда отката последнего применения (настроек или программ) |

Папка агента переопределяется переменной `ENVSYNC_HOME`, настройки лежат в `settings.json` рядом.
