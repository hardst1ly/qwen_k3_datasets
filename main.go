// TrayTranslator — утилита-переводчик в системном трее.
//
// Alt+E в любом приложении → копирует выделенный текст (с сохранением и
// восстановлением буфера обмена) → открывает Google Translate в браузере по
// умолчанию с уже подставленным текстом. Никаких API-ключей и внешних
// сервисов: только открытие URL.
//
// Сборка (подробности — в README.md):
//
//	Windows: go build -ldflags "-H windowsgui -s -w" -o TrayTranslator.exe .
//	Linux:   go build -ldflags "-s -w" -o traytranslator .
//	macOS:   go build -ldflags "-s -w" -o TrayTranslator .
//
// Весь платформо-зависимый низкоуровневый код (SendInput / XTest / CGEvent,
// буфер обмена Windows, уведомления в трее) находится в cgo-преамбуле ниже и
// разделён через #ifdef — поэтому проект остаётся одним файлом.
package main

/*
#cgo windows LDFLAGS: -luser32 -lshell32 -lole32 -ladvapi32 -ldwmapi
#cgo darwin LDFLAGS: -framework ApplicationServices -framework CoreFoundation
#cgo linux LDFLAGS: -ldl

#include <stdlib.h>
#include <string.h>

// =====================================================================
// Windows: RegisterHotKey + GetMessage, SendInput, полный снимок буфера
// обмена (текст, картинки, файлы, RTF/HTML), балун-уведомления в трее.
// =====================================================================
#ifdef _WIN32

#undef WINVER
#undef _WIN32_WINNT
#undef _WIN32_IE
#define WINVER 0x0601
#define _WIN32_WINNT 0x0601
#define _WIN32_IE 0x0800
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <shellapi.h>
#include <objbase.h>
#include <dwmapi.h>
#include <wchar.h>

#ifndef MOD_NOREPEAT
#define MOD_NOREPEAT 0x4000
#endif
#ifndef DWMWA_CLOAKED
#define DWMWA_CLOAKED 14
#endif
#ifndef NIIF_NOSOUND
#define NIIF_NOSOUND 0x00000010
#endif

#define TT_HOTKEY_ID 0x7A11
#define TT_MASK_VK   0xE8   // неназначенная виртуальная клавиша (как в AutoHotkey)

// Один экземпляр программы на сессию пользователя.
static int tt_single_instance(void) {
	HANDLE m = CreateMutexW(NULL, TRUE, L"Local\\TrayTranslator_AltE");
	if (m == NULL) return 1;
	if (GetLastError() == ERROR_ALREADY_EXISTS) return 0;
	return 1; // хэндл держим открытым до конца процесса
}

// Регистрация глобального хоткея Alt+E через системный API.
// Вызывается и ждётся на одном и том же (закреплённом) потоке.
static int tt_hotkey_register(void) {
	MSG msg;
	PeekMessageW(&msg, NULL, WM_USER, WM_USER, PM_NOREMOVE); // создаём очередь сообщений потока
	return RegisterHotKey(NULL, TT_HOTKEY_ID, MOD_ALT | MOD_NOREPEAT, 'E') ? 1 : -1; // -1: сочетание занято
}

// Блокирующее ожидание WM_HOTKEY (без polling — поток спит в GetMessage).
static int tt_hotkey_wait(void) {
	MSG msg;
	for (;;) {
		BOOL r = GetMessageW(&msg, NULL, 0, 0);
		if (r <= 0) return 0;
		if (msg.message == WM_HOTKEY && msg.wParam == TT_HOTKEY_ID) return 1;
		DispatchMessageW(&msg);
	}
}

static int tt_is_down(int vk) { return (GetAsyncKeyState(vk) & 0x8000) != 0; }

static void tt_key(INPUT *in, WORD vk, DWORD flags) {
	memset(in, 0, sizeof(*in));
	in->type = INPUT_KEYBOARD;
	in->ki.wVk = vk;
	in->ki.wScan = (WORD)MapVirtualKeyW(vk, 0); // MAPVK_VK_TO_VSC
	in->ki.dwFlags = flags;
}

// Эмуляция Ctrl+C через SendInput.
// Если вызвано по хоткею — сначала «маскируем» Alt (иначе его отпускание
// активирует меню окна) и дожидаемся отпускания Alt/E, чтобы не получилось
// Ctrl+Alt+C.
static int tt_send_copy(int fromHotkey) {
	INPUT in[8];
	int n = 0;
	if (fromHotkey) {
		if (tt_is_down(VK_MENU)) {
			tt_key(&in[n++], TT_MASK_VK, 0);
			tt_key(&in[n++], TT_MASK_VK, KEYEVENTF_KEYUP);
			SendInput(n, in, sizeof(INPUT));
			n = 0;
		}
		for (int i = 0; i < 60 && (tt_is_down('E') || tt_is_down(VK_MENU)); i++) Sleep(10);
		if (tt_is_down(VK_LMENU)) tt_key(&in[n++], VK_LMENU, KEYEVENTF_KEYUP);
		if (tt_is_down(VK_RMENU)) tt_key(&in[n++], VK_RMENU, KEYEVENTF_KEYUP | KEYEVENTF_EXTENDEDKEY);
	}
	tt_key(&in[n++], VK_CONTROL, 0);
	tt_key(&in[n++], 'C', 0);
	tt_key(&in[n++], 'C', KEYEVENTF_KEYUP);
	tt_key(&in[n++], VK_CONTROL, KEYEVENTF_KEYUP);
	return SendInput(n, in, sizeof(INPUT)) == (UINT)n ? 1 : 0;
}

// Проверка UIPI: активное окно запущено с правами выше, чем у утилиты?
// В этом случае Windows молча блокирует SendInput в это окно.
static int tt_token_elevated(HANDLE proc, int *known) {
	HANDLE tok = NULL;
	TOKEN_ELEVATION te;
	DWORD sz = 0;
	*known = 0;
	if (!OpenProcessToken(proc, TOKEN_QUERY, &tok)) return GetLastError() == ERROR_ACCESS_DENIED;
	int r = 0;
	if (GetTokenInformation(tok, TokenElevation, &te, sizeof(te), &sz)) { *known = 1; r = te.TokenIsElevated != 0; }
	CloseHandle(tok);
	return r;
}

static int tt_foreground_elevated(void) {
	int known = 0;
	if (tt_token_elevated(GetCurrentProcess(), &known) && known) return 0; // мы сами администратор
	HWND h = GetForegroundWindow();
	if (!h) return 0;
	DWORD pid = 0;
	GetWindowThreadProcessId(h, &pid);
	if (!pid || pid == GetCurrentProcessId()) return 0;
	HANDLE p = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid);
	if (!p) return GetLastError() == ERROR_ACCESS_DENIED;
	int r = tt_token_elevated(p, &known);
	CloseHandle(p);
	return r;
}

// ---- Буфер обмена: полный снимок всех HGLOBAL-форматов ----
typedef struct { UINT fmt; HGLOBAL data; } tt_clip_item;
typedef struct { int n, cap; tt_clip_item *items; } tt_clip_snap;

static int tt_open_clip(void) {
	for (int i = 0; i < 25; i++) {           // до ~200 мс, если буфер занят другим процессом
		if (OpenClipboard(NULL)) return 1;
		Sleep(8);
	}
	return 0;
}

static int tt_fmt_copyable(UINT f) {
	switch (f) {
	case CF_BITMAP: case CF_METAFILEPICT: case CF_PALETTE: case CF_ENHMETAFILE:
	case CF_OWNERDISPLAY: case CF_DSPBITMAP: case CF_DSPMETAFILEPICT: case CF_DSPENHMETAFILE:
		return 0; // GDI-хэндлы, не HGLOBAL (CF_BITMAP система синтезирует из CF_DIB)
	}
	if (f >= CF_PRIVATEFIRST && f <= CF_PRIVATELAST) return 0;
	if (f >= CF_GDIOBJFIRST && f <= CF_GDIOBJLAST) return 0;
	return 1;
}

static void *tt_clip_save(void) {
	if (!tt_open_clip()) return NULL;
	tt_clip_snap *s = (tt_clip_snap *)calloc(1, sizeof(tt_clip_snap));
	UINT f = 0;
	while (s && (f = EnumClipboardFormats(f)) != 0) {
		if (!tt_fmt_copyable(f)) continue;
		HANDLE h = GetClipboardData(f);
		if (!h) continue;
		SIZE_T sz = GlobalSize(h);
		if (sz == 0) continue;
		void *src = GlobalLock(h);
		if (!src) continue;
		HGLOBAL cp = GlobalAlloc(GMEM_MOVEABLE, sz);
		if (cp) {
			void *dst = GlobalLock(cp);
			if (dst) { memcpy(dst, src, sz); GlobalUnlock(cp); }
			else { GlobalFree(cp); cp = NULL; }
		}
		GlobalUnlock(h);
		if (!cp) continue;
		if (s->n == s->cap) {
			int nc = s->cap ? s->cap * 2 : 8;
			tt_clip_item *ni = (tt_clip_item *)realloc(s->items, nc * sizeof(tt_clip_item));
			if (!ni) { GlobalFree(cp); break; }
			s->items = ni; s->cap = nc;
		}
		s->items[s->n].fmt = f;
		s->items[s->n].data = cp;
		s->n++;
	}
	CloseClipboard();
	return s;
}

static void tt_clip_clear(void) {
	if (tt_open_clip()) { EmptyClipboard(); CloseClipboard(); }
}

static unsigned int tt_clip_seq(void) { return (unsigned int)GetClipboardSequenceNumber(); }

// Текст из буфера в UTF-8 (NULL, если текста нет). Освобождать через free().
static char *tt_clip_text(void) {
	if (!IsClipboardFormatAvailable(CF_UNICODETEXT)) return NULL;
	if (!tt_open_clip()) return NULL;
	char *out = NULL;
	HANDLE h = GetClipboardData(CF_UNICODETEXT);
	if (h) {
		SIZE_T cap = GlobalSize(h) / sizeof(WCHAR);
		const WCHAR *w = (const WCHAR *)GlobalLock(h);
		if (w && cap > 0) {
			int wl = (int)wcsnlen(w, cap);
			int n = WideCharToMultiByte(CP_UTF8, 0, w, wl, NULL, 0, NULL, NULL);
			out = (char *)malloc((size_t)n + 1);
			if (out) {
				if (n > 0) WideCharToMultiByte(CP_UTF8, 0, w, wl, out, n, NULL, NULL);
				out[n] = 0;
			}
		}
		if (w) GlobalUnlock(h);
	}
	CloseClipboard();
	return out;
}

// Восстановление снимка. После успешного SetClipboardData память принадлежит системе.
static void tt_clip_restore(void *p) {
	tt_clip_snap *s = (tt_clip_snap *)p;
	if (!s) return;
	int opened = tt_open_clip();
	if (opened) EmptyClipboard();
	for (int i = 0; i < s->n; i++) {
		if (!opened || !SetClipboardData(s->items[i].fmt, s->items[i].data)) GlobalFree(s->items[i].data);
	}
	if (opened) CloseClipboard();
	free(s->items);
	free(s);
}

// Балун-уведомление от иконки трея (окно "SystrayClass", uID=100 — из fyne.io/systray).
static int tt_balloon(const char *title, const char *msg) {
	HWND h = NULL;
	DWORD me = GetCurrentProcessId();
	while ((h = FindWindowExW(NULL, h, L"SystrayClass", NULL)) != NULL) {
		DWORD pid = 0;
		GetWindowThreadProcessId(h, &pid);
		if (pid == me) break;
	}
	if (!h) return 0;
	NOTIFYICONDATAW nid;
	memset(&nid, 0, sizeof(nid));
	nid.cbSize = sizeof(nid);
	nid.hWnd = h;
	nid.uID = 100;
	nid.uFlags = NIF_INFO;
	nid.dwInfoFlags = NIIF_INFO | NIIF_NOSOUND;
	MultiByteToWideChar(CP_UTF8, 0, title, -1, nid.szInfoTitle, 63);
	MultiByteToWideChar(CP_UTF8, 0, msg, -1, nid.szInfo, 255);
	return Shell_NotifyIconW(NIM_MODIFY, &nid) ? 1 : 0;
}

// Открытие URL в браузере по умолчанию (ShellExecute, без мигания консоли).
static int tt_open_url(const char *url) {
	int n = MultiByteToWideChar(CP_UTF8, 0, url, -1, NULL, 0);
	if (n <= 0) return 0;
	WCHAR *w = (WCHAR *)malloc((size_t)n * sizeof(WCHAR));
	if (!w) return 0;
	MultiByteToWideChar(CP_UTF8, 0, url, -1, w, n);
	HRESULT hr = CoInitializeEx(NULL, COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE);
	AllowSetForegroundWindow(ASFW_ANY); // браузер может выйти на передний план
	HINSTANCE r = ShellExecuteW(NULL, L"open", w, NULL, NULL, SW_SHOWNORMAL);
	if (SUCCEEDED(hr)) CoUninitialize();
	free(w);
	return (INT_PTR)r > 32;
}

// Пункт меню трея забирает фокус себе. Возвращаем фокус окну, которое
// было активным до клика (первое «настоящее» окно в Z-порядке).
static BOOL CALLBACK tt_enum_prev(HWND h, LPARAM lp) {
	if (!IsWindowVisible(h) || IsIconic(h)) return TRUE;
	DWORD pid = 0;
	GetWindowThreadProcessId(h, &pid);
	if (pid == GetCurrentProcessId()) return TRUE;
	if (GetWindowLongPtrW(h, GWL_EXSTYLE) & WS_EX_TOOLWINDOW) return TRUE;
	WCHAR cls[128];
	cls[0] = 0;
	GetClassNameW(h, cls, 128);
	static const WCHAR *skip[] = { L"Shell_TrayWnd", L"Shell_SecondaryTrayWnd", L"Progman", L"WorkerW",
		L"NotifyIconOverflowWindow", L"TopLevelWindowForOverflowXamlIsland", L"Windows.UI.Core.CoreWindow", NULL };
	for (int i = 0; skip[i]; i++) if (wcscmp(cls, skip[i]) == 0) return TRUE;
	BOOL cloaked = FALSE;
	if (SUCCEEDED(DwmGetWindowAttribute(h, DWMWA_CLOAKED, &cloaked, sizeof(cloaked))) && cloaked) return TRUE;
	if (GetWindowTextLengthW(h) == 0) return TRUE;
	*(HWND *)lp = h;
	return FALSE;
}

static int tt_focus_prev(void) {
	HWND h = NULL;
	EnumWindows(tt_enum_prev, (LPARAM)&h);
	if (!h) return 0;
	SetForegroundWindow(h);
	return 1;
}

#else // ---- заглушки Windows-функций для остальных ОС ----

static int tt_single_instance(void) { return 1; }
static int tt_foreground_elevated(void) { return 0; }
static void *tt_clip_save(void) { return NULL; }
static void tt_clip_clear(void) {}
static unsigned int tt_clip_seq(void) { return 0; }
static char *tt_clip_text(void) { return NULL; }
static void tt_clip_restore(void *p) { (void)p; }
static int tt_balloon(const char *t, const char *m) { (void)t; (void)m; return 0; }
static int tt_open_url(const char *u) { (void)u; return 0; }
static int tt_focus_prev(void) { return 0; }

#endif

// =====================================================================
// macOS: CGEvent (Cmd+C) и запрос разрешения Accessibility.
// =====================================================================
#ifdef __APPLE__
#include <ApplicationServices/ApplicationServices.h>
#include <CoreFoundation/CoreFoundation.h>
#include <dispatch/dispatch.h>
#include <unistd.h>

// Глобальный хоткей ⌥E через CGEventTap (системный перехват событий, без
// опроса). RegisterEventHotKey в macOS 15+ не принимает сочетания только с
// Option, поэтому используется tap — ему нужен «Универсальный доступ».
static dispatch_semaphore_t tt_mac_sem = NULL;
static CFMachPortRef tt_mac_tap = NULL;
static int tt_mac_swallow_up = 0;

static CGEventRef tt_mac_tap_cb(CGEventTapProxy proxy, CGEventType type, CGEventRef ev, void *ref) {
	(void)proxy; (void)ref;
	if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
		if (tt_mac_tap) CGEventTapEnable(tt_mac_tap, true);
		return ev;
	}
	if (type != kCGEventKeyDown && type != kCGEventKeyUp) return ev;
	if (CGEventGetIntegerValueField(ev, kCGKeyboardEventKeycode) != 14) return ev; // kVK_ANSI_E
	if (type == kCGEventKeyDown) {
		CGEventFlags f = CGEventGetFlags(ev);
		int opt = (f & kCGEventFlagMaskAlternate) != 0;
		int other = (f & (kCGEventFlagMaskCommand | kCGEventFlagMaskControl | kCGEventFlagMaskShift)) != 0;
		if (opt && !other) {
			if (!CGEventGetIntegerValueField(ev, kCGKeyboardEventAutorepeat)) dispatch_semaphore_signal(tt_mac_sem);
			tt_mac_swallow_up = 1;
			return NULL; // «съедаем» ⌥E, чтобы в приложение не попал символ «´»
		}
	} else if (tt_mac_swallow_up) {
		tt_mac_swallow_up = 0;
		return NULL;
	}
	return ev;
}

static void tt_mac_install(void *ctx) {
	int *ok = (int *)ctx;
	CGEventMask mask = CGEventMaskBit(kCGEventKeyDown) | CGEventMaskBit(kCGEventKeyUp);
	tt_mac_tap = CGEventTapCreate(kCGSessionEventTap, kCGHeadInsertEventTap, kCGEventTapOptionDefault, mask, tt_mac_tap_cb, NULL);
	if (!tt_mac_tap) { *ok = 0; return; }
	CFRunLoopSourceRef src = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, tt_mac_tap, 0);
	CFRunLoopAddSource(CFRunLoopGetMain(), src, kCFRunLoopCommonModes);
	CFRelease(src);
	CGEventTapEnable(tt_mac_tap, true);
	*ok = 1;
}

static int tt_hotkey_register(void) {
	if (!AXIsProcessTrusted()) return 0;
	if (!tt_mac_sem) tt_mac_sem = dispatch_semaphore_create(0);
	int ok = 0;
	dispatch_sync_f(dispatch_get_main_queue(), &ok, tt_mac_install); // tap живёт в главном run loop
	return ok;
}

static int tt_hotkey_wait(void) {
	dispatch_semaphore_wait(tt_mac_sem, DISPATCH_TIME_FOREVER);
	return 1;
}

// prompt=1 — показать системный диалог «Разрешить универсальный доступ».
static int tt_ax_trusted(int prompt) {
	const void *keys[] = { kAXTrustedCheckOptionPrompt };
	const void *vals[] = { prompt ? kCFBooleanTrue : kCFBooleanFalse };
	CFDictionaryRef opts = CFDictionaryCreate(kCFAllocatorDefault, keys, vals, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	Boolean ok = AXIsProcessTrustedWithOptions(opts);
	if (opts) CFRelease(opts);
	return ok ? 1 : 0;
}

// Cmd+C. Флаги события задаются явно, поэтому зажатый Option не мешает.
static int tt_send_copy(int fromHotkey) {
	(void)fromHotkey;
	CGEventSourceRef src = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
	CGEventRef down = CGEventCreateKeyboardEvent(src, (CGKeyCode)8, true);  // kVK_ANSI_C
	CGEventRef up = CGEventCreateKeyboardEvent(src, (CGKeyCode)8, false);
	int ok = down && up;
	if (ok) {
		CGEventSetFlags(down, kCGEventFlagMaskCommand);
		CGEventSetFlags(up, kCGEventFlagMaskCommand);
		CGEventPost(kCGHIDEventTap, down);
		usleep(10000);
		CGEventPost(kCGHIDEventTap, up);
	}
	if (down) CFRelease(down);
	if (up) CFRelease(up);
	if (src) CFRelease(src);
	return ok;
}

#else
static int tt_ax_trusted(int prompt) { (void)prompt; return 1; }
#endif

// =====================================================================
// Linux / X11: XTest (libX11 + libXtst подгружаются через dlopen, поэтому
// для сборки не нужны заголовки X11, а при их отсутствии в системе
// используется fallback xdotool / ydotool / wtype).
// =====================================================================
#if !defined(_WIN32) && !defined(__APPLE__)
#include <dlfcn.h>
#include <unistd.h>

typedef void *(*tt_XOpenDisplay)(const char *);
typedef int (*tt_XCloseDisplay)(void *);
typedef unsigned char (*tt_XKeysymToKeycode)(void *, unsigned long);
typedef int (*tt_XFlush)(void *);
typedef int (*tt_XQueryKeymap)(void *, char *);
typedef int (*tt_XTestFakeKeyEvent)(void *, unsigned int, int, unsigned long);
typedef int (*tt_XTestQueryExtension)(void *, int *, int *, int *, int *);

typedef int (*tt_XErrorHandler)(void *, void *);
typedef tt_XErrorHandler (*tt_XSetErrorHandler)(tt_XErrorHandler);
typedef int (*tt_XGrabKey)(void *, int, unsigned int, unsigned long, int, int, int);
typedef unsigned long (*tt_XDefaultRootWindow)(void *);
typedef int (*tt_XSync)(void *, int);
typedef int (*tt_XNextEvent)(void *, void *);
typedef int (*tt_XInitThreads)(void);
// Совпадает с раскладкой XErrorEvent из Xlib.h.
typedef struct { int type; void *display; unsigned long resourceid; unsigned long serial;
	unsigned char error_code, request_code, minor_code; } tt_XErrorEvent;

static void *x11 = NULL, *xtst = NULL;
static int tt_load_x11(void) {
	if (!x11) x11 = dlopen("libX11.so.6", RTLD_LAZY | RTLD_GLOBAL);
	return x11 != NULL;
}

static int tt_kdown(const char *km, unsigned int kc) { return kc && (km[kc >> 3] & (1 << (kc & 7))); }

// ---- Глобальный хоткей Alt+E: XGrabKey на корневом окне + блокирующий XNextEvent ----
static void *tt_hk_display = NULL;
static tt_XNextEvent tt_pNextEvent = NULL;
static int tt_grab_error = 0;
static int tt_grab_err_handler(void *d, void *ev) { (void)d; tt_grab_error = ((tt_XErrorEvent *)ev)->error_code; return 0; }

// 1 — успех, 0 — нет X-сервера, -1 — сочетание занято другой программой.
static int tt_hotkey_register(void) {
	if (!tt_load_x11()) return 0;
	tt_XInitThreads pInit = (tt_XInitThreads)dlsym(x11, "XInitThreads");
	tt_XOpenDisplay pOpen = (tt_XOpenDisplay)dlsym(x11, "XOpenDisplay");
	tt_XCloseDisplay pClose = (tt_XCloseDisplay)dlsym(x11, "XCloseDisplay");
	tt_XKeysymToKeycode pK2C = (tt_XKeysymToKeycode)dlsym(x11, "XKeysymToKeycode");
	tt_XGrabKey pGrab = (tt_XGrabKey)dlsym(x11, "XGrabKey");
	tt_XDefaultRootWindow pRoot = (tt_XDefaultRootWindow)dlsym(x11, "XDefaultRootWindow");
	tt_XSync pSync = (tt_XSync)dlsym(x11, "XSync");
	tt_XSetErrorHandler pSetErr = (tt_XSetErrorHandler)dlsym(x11, "XSetErrorHandler");
	tt_pNextEvent = (tt_XNextEvent)dlsym(x11, "XNextEvent");
	if (!pInit || !pOpen || !pClose || !pK2C || !pGrab || !pRoot || !pSync || !pSetErr || !tt_pNextEvent) return 0;
	pInit();
	void *d = pOpen(NULL);
	if (!d) return 0;
	int kc = pK2C(d, 0x0065); // XK_e
	unsigned long root = pRoot(d);
	// Mod1 (Alt) + варианты с CapsLock (LockMask) и NumLock (Mod2)
	unsigned int mods[4] = { 1u << 3, (1u << 3) | 2u, (1u << 3) | (1u << 4), (1u << 3) | 2u | (1u << 4) };
	tt_grab_error = 0;
	tt_XErrorHandler old = pSetErr(tt_grab_err_handler);
	for (int i = 0; i < 4; i++) pGrab(d, kc, mods[i], root, 0, 1, 1); // owner_events=False, GrabModeAsync
	pSync(d, 0);
	pSetErr(old);
	if (tt_grab_error == 10) { pClose(d); return -1; } // BadAccess
	tt_hk_display = d;
	return 1;
}

static int tt_hotkey_wait(void) {
	if (!tt_hk_display) return 0;
	long ev[24]; // sizeof(XEvent)
	for (;;) {
		tt_pNextEvent(tt_hk_display, ev);
		if (*(int *)ev == 2) return 1; // KeyPress
	}
}

// ---- Эмуляция Ctrl+C через XTest ----
static int tt_send_copy(int fromHotkey) {
	if (!tt_load_x11()) return 0;
	if (!xtst) xtst = dlopen("libXtst.so.6", RTLD_LAZY);
	if (!xtst) return 0;
	tt_XOpenDisplay pOpen = (tt_XOpenDisplay)dlsym(x11, "XOpenDisplay");
	tt_XCloseDisplay pClose = (tt_XCloseDisplay)dlsym(x11, "XCloseDisplay");
	tt_XKeysymToKeycode pK2C = (tt_XKeysymToKeycode)dlsym(x11, "XKeysymToKeycode");
	tt_XFlush pFlush = (tt_XFlush)dlsym(x11, "XFlush");
	tt_XQueryKeymap pKeymap = (tt_XQueryKeymap)dlsym(x11, "XQueryKeymap");
	tt_XTestFakeKeyEvent pFake = (tt_XTestFakeKeyEvent)dlsym(xtst, "XTestFakeKeyEvent");
	tt_XTestQueryExtension pQuery = (tt_XTestQueryExtension)dlsym(xtst, "XTestQueryExtension");
	if (!pOpen || !pClose || !pK2C || !pFlush || !pKeymap || !pFake || !pQuery) return 0;
	void *d = pOpen(NULL);
	if (!d) return 0;
	int a, b, c, e;
	if (!pQuery(d, &a, &b, &c, &e)) { pClose(d); return 0; }

	unsigned int kCtrl = pK2C(d, 0xffe3); // XK_Control_L
	unsigned int kC    = pK2C(d, 0x0063); // XK_c
	unsigned int kAltL = pK2C(d, 0xffe9); // XK_Alt_L
	unsigned int kAltR = pK2C(d, 0xffea); // XK_Alt_R
	unsigned int kE    = pK2C(d, 0x0065); // XK_e
	if (!kCtrl || !kC) { pClose(d); return 0; }

	if (fromHotkey) {
		// Пока E зажата, XGrabKey держит активный захват клавиатуры —
		// ждём отпускания (до 1 с), затем отпускаем Alt программно.
		char km[32];
		for (int i = 0; i < 100; i++) {
			pKeymap(d, km);
			if (!tt_kdown(km, kE) && !tt_kdown(km, kAltL) && !tt_kdown(km, kAltR)) break;
			usleep(10000);
		}
		pKeymap(d, km);
		if (tt_kdown(km, kAltL)) pFake(d, kAltL, 0, 0);
		if (tt_kdown(km, kAltR)) pFake(d, kAltR, 0, 0);
	}
	pFake(d, kCtrl, 1, 0);
	pFake(d, kC, 1, 0);
	pFake(d, kC, 0, 0);
	pFake(d, kCtrl, 0, 0);
	pFlush(d);
	pClose(d);
	return 1;
}
#endif
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"fyne.io/systray"
	"github.com/atotto/clipboard"
	"github.com/godbus/dbus/v5"
)

const (
	appTitle      = "Переводчик"
	configName    = "translator-config.json"
	defaultLang   = "ru"
	copyTimeout   = 300 * time.Millisecond // сколько ждём, пока приложение отдаст текст по Ctrl+C
	maxTextRunes  = 5000                   // лимит Google Translate на один запрос
	maxEncodedLen = 14000                  // ограничение длины URL (сервер Google и командная строка)
)

// ---------------------------------------------------------------------
// Языки перевода
// ---------------------------------------------------------------------

type language struct{ Code, Name string }

var languages = []language{
	{"ru", "Русский"},
	{"en", "English"},
	{"de", "Deutsch"},
	{"es", "Español"},
	{"fr", "Français"},
	{"uk", "Українська"},
	{"zh-CN", "中文"},
	{"ja", "日本語"},
}

func langName(code string) string {
	for _, l := range languages {
		if l.Code == code {
			return l.Name
		}
	}
	return code
}

// ---------------------------------------------------------------------
// Конфиг: JSON рядом с исполняемым файлом
// ---------------------------------------------------------------------

type config struct {
	TargetLang string `json:"target_lang"`
}

var (
	cfgMu    sync.Mutex
	cfg      = config{TargetLang: defaultLang}
	cfgPath  string
	firstRun bool

	busy      sync.Mutex // защита от повторного срабатывания во время перевода
	langItems []*systray.MenuItem
)

// Путь к конфигу: рядом с бинарником; если папка недоступна для записи
// (например, Program Files) — в пользовательском каталоге настроек.
func resolveConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		p := filepath.Join(dir, configName)
		if _, err := os.Stat(p); err == nil || dirWritable(dir) {
			return p
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "TrayTranslator", configName)
	}
	return configName
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".tt-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func loadConfig() {
	cfgPath = resolveConfigPath()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		firstRun = true
		saveConfig()
		return
	}
	var c config
	if json.Unmarshal(data, &c) == nil && langName(c.TargetLang) != c.TargetLang {
		cfg = c
	}
}

func saveConfig() {
	cfgMu.Lock()
	data, _ := json.MarshalIndent(cfg, "", "  ")
	cfgMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	tmp := cfgPath + ".tmp"
	if os.WriteFile(tmp, append(data, '\n'), 0o644) == nil {
		if os.Rename(tmp, cfgPath) != nil {
			_ = os.WriteFile(cfgPath, append(data, '\n'), 0o644)
			os.Remove(tmp)
		}
	}
}

func currentLang() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.TargetLang
}

// ---------------------------------------------------------------------
// Точка входа
// ---------------------------------------------------------------------

func main() {
	// Режим одного перевода: `TrayTranslator --translate`.
	// Нужен для Wayland, где хоткей назначается в настройках окружения.
	for _, a := range os.Args[1:] {
		switch a {
		case "--translate", "-t":
			loadConfig()
			translateSelection(true)
			return
		case "--help", "-h", "/?":
			fmt.Println("TrayTranslator — переводчик выделенного текста (Alt+E).\n" +
				"  без аргументов   запуск в системном трее\n" +
				"  --translate, -t  перевести выделенный текст один раз и выйти")
			return
		}
	}

	if C.tt_single_instance() == 0 {
		return // уже запущен
	}
	loadConfig()
	systray.Run(onReady, func() {})
}

// ---------------------------------------------------------------------
// Трей и меню
// ---------------------------------------------------------------------

func onReady() {
	systray.SetIcon(trayIcon())
	systray.SetTooltip(tooltip())

	mTranslate := systray.AddMenuItem("Перевести выделенное", "Перевести выделенный текст (Alt+E)")
	mLang := systray.AddMenuItem("Язык перевода", "Выбор языка, на который переводить")
	cur := currentLang()
	for _, l := range languages {
		item := mLang.AddSubMenuItemCheckbox(fmt.Sprintf("%s (%s)", l.Name, l.Code), "", l.Code == cur)
		langItems = append(langItems, item)
	}
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Закрыть переводчик")

	go func() {
		for range mTranslate.ClickedCh {
			go translateFromMenu()
		}
	}()
	for i := range languages {
		go func(i int) {
			for range langItems[i].ClickedCh {
				setLanguage(i)
			}
		}(i)
	}
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	go registerHotkey()

	if firstRun {
		notify(appTitle, "Работаю в трее. Выделите текст и нажмите Alt+E.\nЯзык перевода: "+langName(currentLang()))
	}
}

func tooltip() string {
	return appTitle + " (Alt+E) → " + langName(currentLang())
}

// Смена языка: галочка, сохранение в конфиг, уведомление.
func setLanguage(idx int) {
	l := languages[idx]
	cfgMu.Lock()
	cfg.TargetLang = l.Code
	cfgMu.Unlock()
	for i, item := range langItems {
		if i == idx {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
	saveConfig()
	systray.SetTooltip(tooltip())
	notify(appTitle, "Язык перевода: "+l.Name)
}

// ---------------------------------------------------------------------
// Глобальный хоткей Alt+E (системный API, без опроса клавиатуры)
// ---------------------------------------------------------------------

func registerHotkey() {
	// Регистрация и ожидание идут на одном закреплённом потоке ОС:
	// RegisterHotKey/GetMessage (Windows) и соединение X11 привязаны к потоку.
	runtime.LockOSThread()

	switch runtime.GOOS {
	case "darwin":
		// CGEventTap требует разрешения «Универсальный доступ» (Accessibility).
		if C.tt_ax_trusted(1) == 0 {
			notify(appTitle, "Разрешите доступ: Системные настройки → Конфиденциальность и безопасность → Универсальный доступ.")
		}
		for C.tt_hotkey_register() != 1 {
			time.Sleep(2 * time.Second) // ждём, пока пользователь выдаст разрешение
		}

	case "windows":
		if C.tt_hotkey_register() != 1 {
			notify(appTitle, "Не удалось зарегистрировать Alt+E: сочетание занято другой программой.")
			return
		}

	default: // Linux / *BSD
		if isWayland() {
			// Wayland: глобальные хоткеи доступны только через портал
			// org.freedesktop.portal.GlobalShortcuts (KDE Plasma 5.27+, GNOME 48+, Hyprland).
			if err := portalHotkey(); err == nil {
				return
			}
		}
		switch C.tt_hotkey_register() { // X11 / XWayland: XGrabKey
		case -1:
			notify(appTitle, "Не удалось зарегистрировать Alt+E: сочетание занято другой программой.")
			return
		case 0:
			notify(appTitle, "Глобальный Alt+E недоступен (нет X11). Назначьте в настройках системы сочетание на команду:\n"+selfCommand()+" --translate")
			return
		}
		if isWayland() {
			notify(appTitle, "Wayland: Alt+E работает только в X11-окнах. Для всех окон назначьте системное сочетание на:\n"+selfCommand()+" --translate")
		}
	}

	for C.tt_hotkey_wait() == 1 {
		go translateSelection(true)
	}
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}

func selfCommand() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "traytranslator"
}

// Регистрация хоткея через XDG Desktop Portal (GlobalShortcuts) по D-Bus.
func portalHotkey() error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return err
	}
	names := conn.Names()
	if len(names) == 0 {
		return fmt.Errorf("нет имени на шине D-Bus")
	}
	sender := strings.ReplaceAll(strings.TrimPrefix(names[0], ":"), ".", "_")
	portal := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	signals := make(chan *dbus.Signal, 32)
	conn.Signal(signals)
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.portal.Request"), dbus.WithMatchMember("Response")); err != nil {
		return err
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.portal.GlobalShortcuts"), dbus.WithMatchMember("Activated")); err != nil {
		return err
	}

	token := func() string { return fmt.Sprintf("tt%d", rand.Int63()) }
	// Ждём Response на объекте запроса (путь известен заранее по токену).
	await := func(tok string, timeout time.Duration) (map[string]dbus.Variant, error) {
		path := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + tok)
		deadline := time.After(timeout)
		for {
			select {
			case s := <-signals:
				if s.Path != path || s.Name != "org.freedesktop.portal.Request.Response" || len(s.Body) < 2 {
					continue
				}
				if code, _ := s.Body[0].(uint32); code != 0 {
					return nil, fmt.Errorf("портал отклонил запрос (код %d)", code)
				}
				res, _ := s.Body[1].(map[string]dbus.Variant)
				return res, nil
			case <-deadline:
				return nil, fmt.Errorf("портал не ответил")
			}
		}
	}

	t1, sessTok := token(), token()
	call := portal.Call("org.freedesktop.portal.GlobalShortcuts.CreateSession", 0, map[string]dbus.Variant{
		"handle_token":         dbus.MakeVariant(t1),
		"session_handle_token": dbus.MakeVariant(sessTok),
	})
	if call.Err != nil {
		return call.Err
	}
	res, err := await(t1, 10*time.Second)
	if err != nil {
		return err
	}
	var session dbus.ObjectPath
	switch v := res["session_handle"].Value().(type) {
	case string:
		session = dbus.ObjectPath(v)
	case dbus.ObjectPath:
		session = v
	default:
		return fmt.Errorf("портал не вернул сессию")
	}

	type shortcut struct {
		ID      string
		Options map[string]dbus.Variant
	}
	t2 := token()
	call = portal.Call("org.freedesktop.portal.GlobalShortcuts.BindShortcuts", 0, session, []shortcut{{
		ID: "translate",
		Options: map[string]dbus.Variant{
			"description":       dbus.MakeVariant("Перевести выделенный текст"),
			"preferred_trigger": dbus.MakeVariant("ALT+e"),
		},
	}}, "", map[string]dbus.Variant{"handle_token": dbus.MakeVariant(t2)})
	if call.Err != nil {
		return call.Err
	}
	// Окружение может показать пользователю диалог подтверждения — ждём дольше.
	if _, err := await(t2, 2*time.Minute); err != nil {
		return err
	}

	go func() {
		for s := range signals {
			if s.Name == "org.freedesktop.portal.GlobalShortcuts.Activated" && len(s.Body) >= 2 {
				if id, _ := s.Body[1].(string); id == "translate" {
					go translateSelection(true)
				}
			}
		}
	}()
	return nil
}

// ---------------------------------------------------------------------
// Логика перевода
// ---------------------------------------------------------------------

func translateFromMenu() {
	// Меню трея забрало фокус: возвращаем его предыдущему окну.
	if runtime.GOOS == "windows" {
		C.tt_focus_prev()
	}
	time.Sleep(200 * time.Millisecond)
	translateSelection(false)
}

func translateSelection(fromHotkey bool) {
	if !busy.TryLock() {
		return // предыдущий перевод ещё выполняется
	}
	defer busy.Unlock()

	text := strings.TrimSpace(copySelection(fromHotkey))
	if text == "" {
		return // ничего не выделено — молча выходим
	}
	openURL(buildURL(text, currentLang()))
}

// Сохранить буфер → очистить → Ctrl+C → дождаться текста (≤300 мс) → восстановить буфер.
func copySelection(fromHotkey bool) string {
	if runtime.GOOS == "windows" {
		return copySelectionWindows(fromHotkey)
	}

	// 1. Сохраняем текущее содержимое буфера.
	orig, origErr := clipboard.ReadAll()
	// 2. Очищаем буфер.
	_ = clipboard.WriteAll("")
	// 3–5. Ctrl+C / Cmd+C и ожидание текста с таймаутом.
	text := ""
	if sendCopy(fromHotkey) {
		deadline := time.Now().Add(copyTimeout)
		for time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
			if s, err := clipboard.ReadAll(); err == nil && s != "" {
				text = s
				break
			}
		}
	}
	// 6. Восстанавливаем исходное содержимое.
	if origErr == nil {
		_ = clipboard.WriteAll(orig)
	}
	return text
}

func copySelectionWindows(fromHotkey bool) string {
	// Окно с правами администратора: SendInput будет заблокирован UIPI.
	if C.tt_foreground_elevated() == 1 {
		notify(appTitle, "Активное окно запущено от имени администратора — Windows блокирует копирование. Запустите переводчик от администратора.")
		return ""
	}
	// 1. Снимок всех форматов буфера (текст, картинки, файлы, RTF...).
	snap := C.tt_clip_save()
	// 2. Очистка буфера.
	C.tt_clip_clear()
	seq := C.tt_clip_seq()
	// 3. Ctrl+C через SendInput.
	text := ""
	if C.tt_send_copy(boolToC(fromHotkey)) == 1 {
		// 4–5. Ждём появления текста, но не дольше 300 мс (не зависаем).
		deadline := time.Now().Add(copyTimeout)
		for time.Now().Before(deadline) {
			time.Sleep(15 * time.Millisecond)
			if C.tt_clip_seq() == seq {
				continue
			}
			if p := C.tt_clip_text(); p != nil {
				text = C.GoString(p)
				C.free(unsafe.Pointer(p))
				if text != "" {
					break
				}
			}
		}
	}
	// 6. Восстановление исходного содержимого буфера.
	C.tt_clip_restore(snap)
	return text
}

func boolToC(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Эмуляция копирования на Linux/macOS с fallback для Wayland.
func sendCopy(fromHotkey bool) bool {
	if runtime.GOOS == "darwin" {
		return C.tt_send_copy(boolToC(fromHotkey)) == 1
	}
	if isWayland() {
		if fromHotkey {
			time.Sleep(150 * time.Millisecond) // даём отпустить Alt+E
		}
		// ydotool (uinput, нужен запущенный ydotoold): 29 = LeftCtrl, 46 = C.
		if runCmd("ydotool", "key", "29:1", "46:1", "46:0", "29:0") {
			return true
		}
		// wtype — для wlroots-композиторов (Sway, Hyprland).
		if runCmd("wtype", "-M", "ctrl", "c", "-m", "ctrl") {
			return true
		}
	}
	if C.tt_send_copy(boolToC(fromHotkey)) == 1 { // XTest (X11 / XWayland)
		return true
	}
	return runCmd("xdotool", "key", "--clearmodifiers", "ctrl+c")
}

func runCmd(name string, args ...string) bool {
	path, err := exec.LookPath(name)
	if err != nil {
		return false
	}
	cmd := exec.Command(path, args...)
	done := make(chan error, 1)
	if cmd.Start() != nil {
		return false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		return false
	}
}

// URL Google Translate: sl=auto — автоопределение, tl — целевой язык, op=translate.
func buildURL(text, lang string) string {
	r := []rune(text)
	if len(r) > maxTextRunes {
		r = r[:maxTextRunes]
	}
	enc := func(rs []rune) string { return strings.ReplaceAll(url.QueryEscape(string(rs)), "+", "%20") }
	e := enc(r)
	for len(e) > maxEncodedLen && len(r) > 1 {
		r = r[:len(r)*9/10]
		e = enc(r)
	}
	return "https://translate.google.com/?sl=auto&tl=" + url.QueryEscape(lang) + "&text=" + e + "&op=translate"
}

// Открыть URL в браузере по умолчанию.
func openURL(u string) {
	switch runtime.GOOS {
	case "windows":
		cs := C.CString(u)
		C.tt_open_url(cs)
		C.free(unsafe.Pointer(cs))
	case "darwin":
		startDetached("open", u)
	default:
		startDetached("xdg-open", u)
	}
}

func startDetached(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// ---------------------------------------------------------------------
// Уведомления
// ---------------------------------------------------------------------

func notify(title, body string) {
	switch runtime.GOOS {
	case "windows":
		ct, cb := C.CString(title), C.CString(body)
		C.tt_balloon(ct, cb)
		C.free(unsafe.Pointer(ct))
		C.free(unsafe.Pointer(cb))
	case "darwin":
		q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
		startDetached("osascript", "-e", "display notification "+q(body)+" with title "+q(title))
	default:
		if conn, err := dbus.SessionBus(); err == nil {
			obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
			if obj.Call("org.freedesktop.Notifications.Notify", 0, "TrayTranslator", uint32(0),
				"accessories-dictionary", title, body, []string{}, map[string]dbus.Variant{}, int32(4000)).Err == nil {
				return
			}
		}
		startDetached("notify-send", "-a", "TrayTranslator", title, body)
	}
}

// ---------------------------------------------------------------------
// Иконка трея: рисуется в коде (без внешних файлов)
// ---------------------------------------------------------------------

func trayIcon() []byte {
	if runtime.GOOS == "windows" {
		return encodeICO([]int{16, 20, 24, 32, 48, 64})
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, renderIcon(64))
	return buf.Bytes()
}

// Синий скруглённый квадрат с белой буквой «A» и стрелкой перевода.
func renderIcon(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	aa := 1.2 / s
	clamp := func(v float64) float64 { return math.Max(0, math.Min(1, v)) }
	segDist := func(px, py, ax, ay, bx, by float64) float64 {
		dx, dy := bx-ax, by-ay
		t := clamp(((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy))
		return math.Hypot(px-ax-t*dx, py-ay-t*dy)
	}
	type seg struct{ ax, ay, bx, by float64 }
	glyph := []seg{
		{0.24, 0.74, 0.42, 0.24}, {0.42, 0.24, 0.60, 0.74}, {0.31, 0.56, 0.53, 0.56}, // «A»
		{0.62, 0.30, 0.80, 0.30}, {0.73, 0.22, 0.80, 0.30}, {0.73, 0.38, 0.80, 0.30}, // «→»
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := (float64(x)+0.5)/s, (float64(y)+0.5)/s
			// скруглённый квадрат (SDF)
			qx, qy := math.Abs(px-0.5)-0.46+0.18, math.Abs(py-0.5)-0.46+0.18
			d := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - 0.18
			bgA := clamp(0.5 - d/aa)
			if bgA == 0 {
				continue
			}
			dg := 1.0
			for i, g := range glyph {
				w := 0.055
				if i >= 3 {
					w = 0.04
				}
				dg = math.Min(dg, segDist(px, py, g.ax, g.ay, g.bx, g.by)-w)
			}
			gA := clamp(0.5 - dg/aa)
			r := (26 + (66-26)*py) * (1 - gA)
			gc := (115 + (133-115)*py) * (1 - gA)
			b := (232 + (244-232)*py) * (1 - gA)
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r + 255*gA), G: uint8(gc + 255*gA), B: uint8(b + 255*gA), A: uint8(255 * bgA),
			})
		}
	}
	return img
}

// ICO (32-bit BMP) из нескольких размеров — формат, который ждёт Windows.
func encodeICO(sizes []int) []byte {
	var head, body bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&head, le, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for _, sz := range sizes {
		img := renderIcon(sz)
		maskStride := ((sz + 31) / 32) * 4
		var bmp bytes.Buffer
		binary.Write(&bmp, le, struct {
			Size                   uint32
			Width, Height          int32
			Planes, BitCount       uint16
			Compression, SizeImage uint32
			XPels, YPels           int32
			ClrUsed, ClrImportant  uint32
		}{40, int32(sz), int32(sz * 2), 1, 32, 0, uint32(sz*sz*4 + maskStride*sz), 0, 0, 0, 0})
		for y := sz - 1; y >= 0; y-- { // строки снизу вверх, BGRA
			for x := 0; x < sz; x++ {
				c := img.NRGBAAt(x, y)
				bmp.Write([]byte{c.B, c.G, c.R, c.A})
			}
		}
		for y := sz - 1; y >= 0; y-- { // AND-маска: 1 = прозрачный пиксель
			row := make([]byte, maskStride)
			for x := 0; x < sz; x++ {
				if img.NRGBAAt(x, y).A == 0 {
					row[x/8] |= 0x80 >> (x % 8)
				}
			}
			bmp.Write(row)
		}
		w := byte(sz)
		if sz >= 256 {
			w = 0
		}
		head.Write([]byte{w, w, 0, 0})
		binary.Write(&head, le, [2]uint16{1, 32})
		binary.Write(&head, le, [2]uint32{uint32(bmp.Len()), uint32(offset)})
		offset += bmp.Len()
		body.Write(bmp.Bytes())
	}
	return append(head.Bytes(), body.Bytes()...)
}
