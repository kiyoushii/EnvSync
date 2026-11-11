# EnvSync

Синхронизация среды Windows 11 между двумя машинами: файлы, настройки, оформление, программы.

Статус: **этап 0 (план)**. Кода пока нет. Решения и причины — в [ARCHITECTURE.md](ARCHITECTURE.md).

## Замер среды (только чтение)

На каждой машине, в обычном PowerShell:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\measure.ps1
```

Скрипт ничего не меняет и не отправляет по сети. Отчёт сохраняется в `scripts\measure-<машина>.txt`.
