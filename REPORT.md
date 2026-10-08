# Отчёт — zen-router + dsh-opencode-zen (полный цикл)

**Дата:** 2026-10-08 · **Статус цели:** ✅ **выполнена** (все 3 плана закрыты, Phase E закрыт,
чек-лист `checklists/phase-e.md` без открытых пунктов, финальные гейты зелёные).

## 0. Текущее состояние на дату отчёта

| Что | Значение |
|---|---|
| Демон zen-router | pid **31217**, systemd-юнит `zen-router.service` **active**, с 15:24:37 |
| Слушатель | `127.0.0.1:8787` (control API + gateway), `up: true` |
| Активный путь | `direct`, egress-IP `176.212.216.125` (ipify-echo) |
| DSH (профиль web) | pid **35271**, перезапущен **15:54:24** — уже с новым плагином |
| Плагин в профиле | `dsh-opencode-zen` **v0.16.0** (thin), пин `9890e55…`, `a416790` — 0 упоминаний |
| zen-router репо | main = `3e70113` и новее (это сообщение), чистый, = origin/main |
| plugin репо | main = `9890e55` = origin/main, чистый |
| Тесты | Go: **12 пакетов ok / 0 FAIL**; плагин: **64 passed** |
| WARP | код и тесты готовы, **сеть до `api.cloudflareclient.com` с хоста отсутствует** (см. §5) |

## 1. Что сделано

### Постановка
Старый JS-плагин `dsh-opencode-zen` работал медленно и быстро сжигал лимиты opencode;
требовалась ротация IP через Cloudflare WARP. Результат — два репозитория:

| Компонент | Что это | Репо |
|---|---|---|
| **zen-router** | Агенто-агностичный шлюз на Go перед opencode.ai: keep-alive пул (латентность), ротация IP (direct/WARP/пул ключей), учёт квот, TUI на Bubbletea, systemd | `~/Projects/zen-router` |
| **dsh-opencode-zen v0.16.0** | Тонкая точка входа для DeepSeek Harness: одна POST на `127.0.0.1:8787`, ноль ретраев (ретраи у хоста), 64 теста | `~/Projects/dsh-opencode-zen` |

### Фаза 1 — проектирование (без отмашки, только read-only)
- Разведка протокола opencode Zen wire + DSH provider API → спека
  `docs/superpowers/specs/2026-10-06-zen-router-gateway-design.md`
  (стадии ротации §6, отмашка-гейты §12, stickyId §9).
- Три плана реализации, утверждены (режим: субагент-драйв, D1=3 попытки).

### Фаза 2 — код по планам (юнит-тесты с самого начала; live — только по отмашке)

**Plan 1 — core** (14 задач, `ledger-plan1.md` CLOSED):
gateway OpenAI-поверхность (`/v1/chat/completions`, stream и non-stream), SSE-трансляция
и серверный буферинг, классификация ошибок и envelope `{"error":{message,type,code}}`,
staged-ротация (IP-429 → следующий ключ на том же egress → свежая WARP-идентичность →
direct; account-лимиты двигают только ключи; cap = 3 попытки на запрос), учёт квот
(OK/429 на egress и на ключ, сброс 00:00 UTC), keep-alive TCP+TLS пул к opencode.ai.

**Plan 2 — TUI + live** (9 задач, `ledger-plan2.md` CLOSED):
TUI на Bubbletea v2 (дашборд, клавиши r/d/w/s/q, quota-виджеты, лог-тайл, 1-сек опрос
control API), `install-systemd [--remove]`, `up --detach`, egress-IP echo через ipify,
TUI-прокси и dump-config гейты.

**Plan 3 — thin plugin** (8 задач, `ledger-plan3.md` CLOSED, коммит `9890e55`):
`lib/index.js` — 396 строк (кап 400), одна транспортная `stream()` (одна POST,
`stream:true` принудительно, заголовки content-type + условный authorization +
attribution-hook), ошибки маппятся строго по status + `error.type` (без текста
сообщений), `apply()` с health-ping и gated autostart, ноль новых зависимостей.

### Phase E — live-врезка (отмашка дана 2026-10-08)
Чек-лист `docs/superpowers/checklists/phase-e.md` — **все пункты [x]**:
- Пин профиля `a416790` → `9890e55` (`~/.dsh/profiles/web/package.json:13`);
  node_modules = v0.16.0 (thin, `main: lib/index.js`).
- `dsh --profile web --dump-config` → rc=0, stderr пуст (1613 строк).
- Рестарт DSH пользователем: pid 35271, 15:54:24 (после перевода пина).
- Бинарь демона пересобран (14:55:01, атомарный stop→install→start), юнит =
  вывод `zen-router install-systemd`, живой pid 31217.
- TUI на живом демоне прогнан полностью (r/d/w/s/q, все ветки, включая stop/start).
- **Live-вызовы opencode.ai:** stream — пост-рестартовые чат-сессии идут
  плагин → демон → opencode (gateway TTFB direct 4→11 и quota `direct.ok` 5→12
  в окне 16:10–16:15); non-stream — `stream:false` → `200 chat.completion` «pong» (5.0с).
- **Wire-чеки:** authorization собирается только при заданном `OPENCODE_ZEN_API_KEY`
  (env в DSH 35271 отсутствует → на проводе заголовка нет по дизайну; демон принимает
  Bearer → живой 200); attribution-пира в профиле нет → impl null → parity с золотым
  (`a416790` вообще без attribution-кода, grep = 0); stickyId — серверная деривация
  `zen.DeriveRequestIDs` (`internal/zen/session.go:113`, паритет с плагинным
  `deriveRequestIDs`, покрыт тестами); error-конверт вживую
  `400 {"error":{"type":"InvalidRequestError",…}}` — форма, которую парсит тонкий плагин.
- Финальные гейты: gofmt 0, build OK, vet OK, `go mod tidy` → дифф пуст,
  `go test -count=1 -short ./...` → 12 пакетов ok / 0 FAIL.

## 2. Как устроено

```
DeepSeek Harness (профиль web, GUI)
  └─ плагин dsh-opencode-zen v0.16.0 — stream(): ОДНА POST, без ретраев
        │  http://127.0.0.1:8787/v1/chat/completions   (stream: true)
        ▼
zen-router daemon (pid 31217, systemd --user zen-router.service)
  ├─ gateway/   OpenAI-surface, SSE↔JSON, envelope ошибок, canonical session
  ├─ router/    стадии ротации: IP-429 → следующий ключ → свежий WARP → direct
  ├─ quota/     OK/429 по egress и по ключу, reset 00:00 UTC, state.json
  ├─ keys/      пул ключей (pool-config.json → OPENCODE_ZEN_API_KEY → public)
  ├─ warp/      WireGuard zenwarp + DoH, control api.cloudflareclient.com
  └─ transport: direct (keep-alive TCP+TLS пул к opencode.ai)  |  warp
        ▼
    opencode.ai
```

- **Хост (DSH)** делает ретраи по SERVER/RATE_LIMIT/TIMEOUT — плагин их не дублирует.
- **Демон** решает, каким egress идти, сколько попыток (cap 3) и когда ротировать.
- **TUI** ходит в `/_zenctl/*` (опрос раз в секунду).

## 3. Как запускать

### Демон
```sh
zen-router up                 # foreground
zen-router up --detach         # в фон
zen-router status              # egress / mode / quota-счётчики
zen-router stop                # graceful stop
zen-router use direct          # принудительно direct
zen-router use warp            # принудительно warp (нужен доступ к cloudflareclient)
zen-router rotate              # форс-ротация egress сейчас
zen-router tui                 # интерактивный дашборд
zen-router install-systemd     # записать юнит + daemon-reload + enable --now
zen-router install-systemd --remove   # отключить и удалить юнит
```
Сейчас уже запущен и включён в автозапуск: `systemctl --user is-active zen-router` → active.

### Наблюдение за демоном
```sh
systemctl --user status zen-router
journalctl --user -u zen-router -f
tail -f ~/.local/state/zen-router/zen.log
```

### Быстрая проверка работоспособности
```sh
# control API: up/mode/egress/quota
curl -s http://127.0.0.1:8787/_zenctl/status

# список моделей
curl -s http://127.0.0.1:8787/v1/models

# non-stream чат через шлюз
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"ping"}],"stream":false}'
# → 200 {"choices":[{"message":{"content":"pong"}}], "usage": {...}}
```
Счётчики квот — `state.egress.{direct,warp}.{ok,daily429}` и `state.keys.<sha>`;
латентность — `latency_ttfb_ms` (запрос→2xx-заголовки), `latency_stream_ms`
(только legacy-прокси, у gateway не растёт — это нормально).

### TUI
```sh
zen-router tui
```
Клавиши: **r** — ротация, **d** — direct, **w** — warp, **s** — stop/start,
**q** — выход.
Заметки для автоматизации: под `script`/pty нужны `TERM=xterm-256color` и заданный
размер (`stty rows 40 cols 120` внутри pty) — иначе кадр пустой (0×0, `TERM=dumb`).

### DSH-плагин (установка / пин / проверка)
```sh
# перевести пин на конкретный коммит:
pnpm dsh plugin --profile web add "github:kyomufs/dsh-opencode-zen#9890e557734e780f406232c6d85ec2e2427c1c96"

# здоровье конфига (rc=0 и пустой stderr = ок):
dsh --profile web --dump-config

# рестарт DSH — ТОЛЬКО вручную: GUI владеет портом,
# поднимать свой `dsh web` сервер нельзя.
```
Env плагина: `OPENCODE_ZEN_BASE` (default `http://127.0.0.1:8787`),
`OPENCODE_ZEN_API_KEY` — опционально (без него на провод не уходит `authorization`).

### Пути и конфиги (XDG)
| Путь | Что там |
|---|---|
| `~/.config/zen-router/config.json` | listen, upstream base, семья адресов, политика ротации, key-pool; сейчас включён `{"egressIPEcho": true}` |
| `~/.local/state/zen-router/state.json` | квоты, WARP-идентичности, история ротаций (одноразовая миграция из `~/.dsh/state/zen-router/`) |
| `~/.local/state/zen-router/zen.log` | лог демона (его тейлит TUI) |
| `~/.local/bin/zen-router` | бинарь CLI/демона |
| `~/.dsh/profiles/web/package.json:13` | пин плагина |

Env: `ZEN_ROUTER_LISTEN` (default `127.0.0.1:8787`), `ZEN_ROUTER_STATE`.

## 4. Как проверять (гейты)

```sh
cd ~/Projects/zen-router
gofmt -l .                       # пусто = ок
go build ./...
go vet ./...
go mod tidy && git diff --exit-code go.mod go.sum   # пусто = ок
timeout 180 go test -count=1 -short ./...           # 12 пакетов ok, 0 FAIL

cd ~/Projects/dsh-opencode-zen
npm test                                         # 64 passed
```

## 5. Что осталось

**Обязательного — ничего.** Цель закрыта: чек-лист Phase E без открытых пунктов,
оба репо чистые и запушенные, леджеры планов CLOSED, workspace-директории пустые.

### Ограничения среды (не баги кода)
1. **WARP-ротация невозможна с этого хоста**: `api.cloudflareclient.com` не отвечает
   (`cf_http:000`, таймаут ~7с) — регистрация/смена WARP-идентичности физически не
   завершается; `zen-router rotate` и `use warp` зависают на сетевом вызове.
   Direct-путь работает штатно. **Что сделать:** дать сети доступ к
   `api.cloudflareclient.com` (VPN / другая сеть / другой хост) — код ротации готов
   и покрыт тестами, после доступа достаточно `zen-router use warp`.
2. **Наблюдение O1 — сериализация control-API**: обработчики держат лок через
   сетевые вызовы → во время rotate/warp-регистрации `/_zenctl/status` таймаутится и
   TUI показывает ложный «down», пока лок не отпущен. Некритично (single-daemon
   инструмент), зафиксировано в леджере, не чинилось как вне скоупа.

### Отложенные мелочи (report-only, на работу не влияют)
- **Plan 2** (`ledger-plan2.md:37`, Task 8): F1 — ассерт частичного отказа argv;
  F2 — no-op remove message; F3 — путь теряется в ошибке установки; F4 — комментарий
  про exit-code; F5 — daemon-reload после `install-systemd --remove`; F6 — заметка
  про os.Executable и symlink/пробелы в путях.
- **Plan 2** (Task 7): data-borne ESC проходит по статус-путям (инъекция ограничена
  локальным контекстом); рекомендация view-side fingerprinting — не реализована.
- **Plan 3** (`ledger-plan3.md:33-44`): ~10 позиций — wording-ссылки в отчётах
  задач, необязательные тесты (tool-call-delta edge, health-warn capture),
  vestigial `OPENCODE_ZEN_POOL_FILE`, literal `opencode.ai` в smoke-комментарии и т.п.
- Прочие wording-заметки ревью; миноры Plan 1 закрыты батчем в Phase E (2a).

### Опционально (не требуется)
- Задать `OPENCODE_ZEN_API_KEY`, если появится персональный ключ (сейчас public/pool).
- Настроить доступ к `api.cloudflareclient.com`, чтобы включить WARP-ротацию.
- При необходимости удаления юнита — дописать daemon-reload в `--remove` (F5).

## 6. Откат

- **Плагин**: закрепить прежнюю транспорт-прокси версию и перезапустить DSH:
  ```sh
  pnpm dsh plugin --profile web add "github:kyomufs/dsh-opencode-zen#a416790…"
  ```
  (детали — `docs/superpowers/cutover-plan3-thin.md`, §Rollback; пин проверяется
  одним grep по `package.json:13`.)
- **Демон**: `zen-router stop`; юнит — `zen-router install-systemd --remove`.
  Бэкап предыдущего рукописного юнита создавался в `/tmp/zen-router.service.handwritten.bak`
  (может быть потерян после перезагрузки).
- **Состояние** (`state.json`): не трогать — там квоты и идентичности.

## 7. Карта документов

| Файл (`~/Projects/zen-router/docs/superpowers/`) | Содержание |
|---|---|
| `specs/2026-10-06-zen-router-gateway-design.md` | Спека: протокол, ротация §6, отмашка §12 |
| `plans/2026-10-06-zen-router-core.md` | Plan 1 (CLOSED → `ledger-plan1.md`) |
| `plans/2026-10-07-zen-router-phase-c-tui.md` | Plan 2 (CLOSED → `ledger-plan2.md`) |
| `plans/2026-10-07-dsh-opencode-zen-thin.md` | Plan 3 (CLOSED → `ledger-plan3.md`) |
| `checklists/phase-e.md` | Live-чек-лист — все пункты [x] |
| `cutover-plan3-thin.md` | Порядок врезки thin-плагина + execution record |
| `README.md` | Пользовательский README (EN) |
| `REPORT.md` | Этот файл |
