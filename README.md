# EnvSync

Синхронизация среды Windows 11 между двумя машинами: файлы, настройки, оформление, программы.

Статус: **этап 1 (сеть и сопряжение)**, в работе. План — в [docs/stage-1.md](docs/stage-1.md),
решения и причины — в [ARCHITECTURE.md](ARCHITECTURE.md).

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

Папка агента переопределяется переменной `ENVSYNC_HOME`, настройки лежат в `settings.json` рядом.
