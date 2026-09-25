# TrayTranslator — переводчик из системного трея (Alt+E)

Утилита работает в системном трее. Выделяете текст в любом приложении и жмёте **Alt+E**
(на macOS **⌥E**). Google Translate открывается в браузере по умолчанию, текст уже подставлен:

```
https://translate.google.com/?sl=auto&tl={язык}&text={текст}&op=translate
```

Нет окон, консоли, API-ключей и внешних сервисов. Браузер появляется только в момент перевода.

## Готовые файлы

| Файл | Платформа |
|------|-----------|
| `release/TrayTranslator.exe` | Windows 10/11 x64. Один файл, ничего устанавливать не нужно |
| `release/traytranslator-linux-amd64` | Linux x86-64 (glibc ≥ 2.34) |

Для Windows: скопируйте `TrayTranslator.exe` в любую папку и запустите. В трее появится синяя
иконка «A→». Конфиг `translator-config.json` создаётся рядом с exe.

> Exe без цифровой подписи, поэтому при первом запуске SmartScreen может показать
> «Windows защитила ваш компьютер» → «Подробнее» → «Выполнить в любом случае».

**Автозапуск (Windows):** нажмите `Win+R`, введите `shell:startup` и положите туда ярлык на exe.

## Как пользоваться

* **Alt+E** переводит выделенный текст.
* **Клик по иконке** открывает меню:
  * «Перевести выделенное»;
  * «Язык перевода» — Русский (ru), English (en), Deutsch (de), Español (es), Français (fr),
    Українська (uk), 中文 (zh-CN), 日本語 (ja). Текущий язык отмечен галочкой. При смене
    языка появляется уведомление «Язык перевода: …»;
  * «Выход».
* Выбранный язык сохраняется в `translator-config.json` рядом с бинарником, например
  `{"target_lang": "ru"}`. Если в эту папку нельзя писать (например, `Program Files`),
  конфиг сохраняется в `%APPDATA%\TrayTranslator\` или `~/.config/TrayTranslator/`.
* `TrayTranslator --translate` выполняет один перевод и завершается (это fallback для Wayland, см. ниже).

## Как устроен перевод по Alt+E

1. Сохраняется содержимое буфера обмена. На Windows сохраняются **все форматы**: текст,
   картинки, файлы, RTF и HTML. На Linux и macOS сохраняется только текст.
2. Буфер очищается.
3. В активное окно отправляется Ctrl+C: на Windows через `SendInput`, на Linux через XTest
   (или ydotool/wtype), на macOS через `CGEvent` (Cmd+C).
4. Программа проверяет буфер каждые 15–25 мс и ждёт появления текста **не дольше 300 мс**.
   Если приложение не отвечает на Ctrl+C, утилита не зависает.
5. Исходное содержимое буфера восстанавливается.
6. Если текст пустой (ничего не выделено), программа молча выходит.
7. Открывается URL Google Translate. Длина текста ограничена лимитом Google (5000 символов и
   ~14 КБ URL).

Хоткей регистрируется через системный API и работает по событиям, без опроса:

| ОС | Хоткей | Эмуляция копирования |
|----|--------|----------------------|
| Windows | `RegisterHotKey` + блокирующий `GetMessage` | `SendInput` |
| Linux X11 | `XGrabKey` + блокирующий `XNextEvent` | XTest (`libXtst`, через `dlopen`) |
| Linux Wayland | портал `org.freedesktop.portal.GlobalShortcuts` | `ydotool` / `wtype` |
| macOS | `CGEventTap` | `CGEventPost` (Cmd+C) |

## Крайние случаи и ограничения

* **Windows, окна с правами администратора (UIPI).** Если активное окно запущено «от имени
  администратора», а утилита нет, Windows молча блокирует `SendInput` в это окно, и
  скопировать текст невозможно. Утилита определяет такую ситуацию и показывает уведомление.
  Решение: запускать TrayTranslator тоже от администратора.
* **Ничего не выделено** — буфер восстанавливается, браузер не открывается.
* **Приложение не отдаёт Ctrl+C** (игры, некоторые терминалы, защищённые поля) — программа
  выходит по таймауту 300 мс и ничего не делает.
* **Alt+E занят другой программой** — при запуске появится уведомление.
* **Меню отпускания Alt.** В Windows отпускание Alt после хоткея обычно активирует меню окна.
  Утилита подавляет это «маскирующей» клавишей (тот же приём, что в AutoHotkey) и дожидается
  отпускания Alt/E, чтобы не получилось Ctrl+Alt+C.
* **Второй экземпляр** на Windows не запускается (используется именованный mutex).
* **macOS: разрешение Accessibility.** При первом запуске появится системный запрос. Разрешите
  доступ в «Системные настройки → Конфиденциальность и безопасность → Универсальный доступ».
  Хоткей заработает сразу после выдачи разрешения, программа проверяет его каждые 2 с.
  Чтобы не было иконки в Dock, соберите `.app` с `LSUIElement = true` в `Info.plist`.
* **Linux Wayland.** Глобальные хоткеи и эмуляция ввода в Wayland ограничены намеренно.
  * Хоткей: программа пробует портал **GlobalShortcuts** (KDE Plasma 5.27+, GNOME 48+,
    Hyprland). Окружение может один раз спросить подтверждение. Если портала нет, `XGrabKey`
    сработает только в X11-окнах (XWayland). Универсальный fallback: назначьте в настройках
    системы (GNOME: Настройки → Клавиатура → Собственные сочетания; KDE: Комбинации клавиш)
    сочетание Alt+E на команду `/путь/к/traytranslator-linux-amd64 --translate`.
  * Копирование: `ydotool key 29:1 46:1 46:0 29:0` (нужен запущенный `ydotoold` и доступ к
    `/dev/uinput`). Второй вариант — `wtype` (Sway, Hyprland). Для X11-окон используется XTest.
  * Буфер обмена: нужен `wl-clipboard`.
* **Linux X11:** для буфера обмена нужен `xclip` или `xsel`. Трей работает через
  StatusNotifierItem (в GNOME нужно расширение AppIndicator).

Linux, одной командой:
```bash
sudo apt install xclip wl-clipboard ydotool   # Debian/Ubuntu
```

## Сборка из исходников

Стек — **Go**. Весь код в одном файле `main.go` (плюс `go.mod`/`go.sum`). Платформенный код
(SendInput, XTest, CGEvent, буфер обмена Windows) лежит в cgo-преамбуле и разделён через
`#ifdef`, поэтому нужен C-компилятор. Зависимости: `fyne.io/systray` (трей),
`github.com/atotto/clipboard` (буфер на Linux/macOS), `github.com/godbus/dbus/v5`
(уведомления и портал Wayland).

Требуется **Go ≥ 1.24**.

### Windows
Нужен MinGW-w64 gcc в `PATH`, например [WinLibs](https://winlibs.com/)
(`winget install BrechtSanders.WinLibs.POSIX.UCRT`) или MSYS2.
```bat
set CGO_ENABLED=1
go build -ldflags "-H windowsgui -s -w" -o TrayTranslator.exe .
```
`-H windowsgui` убирает консольное окно.

Кросс-сборка exe из Linux или macOS через [zig](https://ziglang.org/) (так собран `release/TrayTranslator.exe`):
```bash
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="zig cc -target x86_64-windows-gnu" \
  go build -trimpath -ldflags "-H windowsgui -s -w -extldflags=-Wl,--subsystem,windows" \
  -o TrayTranslator.exe .
```

### Linux
Заголовки X11 не нужны: libX11 и libXtst подгружаются при запуске через `dlopen`.
```bash
go build -trimpath -ldflags "-s -w" -o traytranslator .
```

### macOS
Нужны Xcode Command Line Tools (`xcode-select --install`).
```bash
go build -trimpath -ldflags "-s -w" -o TrayTranslator .
```

## Почему Go, а не Rust

По приоритету из задания первым шёл Rust. Но в среде сборки не было доступа к crates.io и
rustup, поэтому собрать и проверить Rust-версию было невозможно. Выбран следующий по
приоритету стек — Go. Он даёт один исполняемый файл без рантайма, и exe собран и проверен
(PE x64, подсистема GUI).
