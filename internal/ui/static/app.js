"use strict";

// Интерфейс EnvSync. Весь вывод идёт через textContent: имена файлов и папок приходят извне и не должны
// превращаться в разметку.

const TABS = [
  ["status", "Состояние"],
  ["decisions", "Решения"],
  ["conflicts", "Конфликты"],
  ["plan", "Настройки"],
  ["journal", "Журнал"],
  ["notsynced", "Не синхронизируется"],
];
let current = location.hash.slice(1) || "status";
let lastStatus = null;

function el(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") e.className = v;
    else if (k === "onclick") e.addEventListener("click", v);
    else e.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

async function api(path, post) {
  const opts = post ? { method: "POST", headers: { "X-Requested-With": "envsync" } } : {};
  const r = await fetch(path, opts);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || r.statusText);
  return body;
}

function ago(iso) {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return s + " с назад";
  if (s < 3600) return Math.round(s / 60) + " мин назад";
  if (s < 86400) return Math.round(s / 3600) + " ч назад";
  return Math.round(s / 86400) + " дн назад";
}
const fmtBytes = (n) => (n > 1 << 30 ? (n / (1 << 30)).toFixed(1) + " ГБ" : n > 1 << 20 ? (n / (1 << 20)).toFixed(1) + " МБ" : n > 1024 ? Math.round(n / 1024) + " КБ" : n + " Б");
const pill = (cls, text) => el("span", { class: "pill " + cls }, text);

const STATES = { idle: "синхронизировано", scanning: "сканирование", "scan-waiting": "ждёт сканирования", syncing: "синхронизируется", "sync-preparing": "подготовка", "sync-waiting": "ждёт очереди", cleaning: "очистка", error: "ошибка", paused: "пауза", unknown: "неизвестно" };

function machineCard(st, isSelf) {
  const stale = !isSelf && Date.now() - new Date(st.at).getTime() > 3 * 60 * 1000;
  const ts = st.tailscale, sy = st.syncthing;
  const level = !sy.running || ts.state !== "Running" ? "bad" : st.paused || (st.guard || []).length ? "warn" : "ok";
  const card = el("div", { class: "card" });
  card.append(
    el("h2", {}, el("span", { class: "dot " + (stale ? "warn" : level) }), st.machine, isSelf ? pill("", "эта машина") : pill(stale ? "warn" : "", stale ? "данные устарели: " + ago(st.at) : "снимок " + ago(st.at))),
    el("div", { class: "muted" }, "Tailscale: ", ts.state, ts.ip ? " (" + ts.ip + ")" : "", " · Syncthing: ", sy.running ? "работает" : "не запущен", st.paused ? " · на паузе" : ""),
  );
  if (sy.devices && sy.devices.length) {
    card.append(el("table", {}, el("tr", {}, ["Устройство", "Связь", "Адрес"].map((h) => el("th", {}, h))),
      sy.devices.map((d) => el("tr", {}, el("td", {}, d.name || d.id.slice(0, 7)), el("td", {}, d.paused ? pill("warn", "пауза") : d.connected ? pill("ok", "подключено") : pill("bad", "нет связи")), el("td", { class: "mono" }, d.address || "")))));
  }
  if (sy.folders && sy.folders.length) {
    card.append(el("table", {}, el("tr", {}, ["Папка", "Состояние", "Файлов", "Ждёт", "Ошибки"].map((h) => el("th", {}, h))),
      sy.folders.map((f) => el("tr", {},
        el("td", {}, f.id, el("div", { class: "path" }, f.path || "")),
        el("td", {}, f.paused ? pill("warn", "пауза") : f.state === "idle" ? pill("ok", STATES.idle) : f.state === "error" ? pill("bad", STATES.error) : pill("", STATES[f.state] || f.state)),
        el("td", { class: "num" }, f.localFiles + " / " + f.globalFiles),
        el("td", { class: "num" }, f.needFiles || f.needDeletes ? f.needFiles + " ф., " + f.needDeletes + " уд., " + fmtBytes(f.needBytes) : "—"),
        el("td", { class: "num" }, f.errors ? el("span", { class: "err" }, f.errors) : "—")))));
  }
  return card;
}

async function viewStatus(root) {
  const s = await api("/api/status");
  lastStatus = s;
  root.append(el("div", { class: "grid" }, machineCard(s.self, true), s.others.map((o) => machineCard(o, false))));
  if (!s.others.length) root.append(el("div", { class: "card muted" }, "Состояния второй машины пока нет: оно появится, когда на ней запущен агент и папка состояний дойдёт сюда."));
}

async function viewDecisions(root) {
  const s = lastStatus || (await api("/api/status"));
  const trips = s.self.guard || [];
  if (!trips.length) {
    root.append(el("div", { class: "banner ok" }, "Решений не требуется. Предохранитель массового удаления ничего не остановил."));
    return;
  }
  for (const t of trips) {
    root.append(el("div", { class: "banner" },
      el("strong", {}, "Папка «" + t.folder + "» остановлена"),
      el("div", {}, "Другая машина удаляет " + t.needDeletes + " из " + t.globalFiles + " файлов (" + ago(t.at) + "). Пока вы не решите, ничего не удаляется."),
      el("div", { class: "actions" },
        el("button", { class: "btn primary", onclick: () => decide("reject", t.folder, "Отклонить удаление и вернуть файлы?") }, "Отклонить: вернуть файлы"),
        el("button", { class: "btn danger", onclick: () => decide("accept", t.folder, "Принять удаление? Файлы останутся в корзине (.stversions).") }, "Принять удаление"))));
  }
}

async function decide(kind, folder, question) {
  if (!confirm(question)) return;
  try {
    const r = await api("/api/guard/" + kind + "?folder=" + encodeURIComponent(folder), true);
    alert(r.result);
  } catch (e) {
    alert("Не удалось: " + e.message);
  }
  render();
}

async function viewConflicts(root) {
  const list = await api("/api/conflicts");
  if (!list.length) return root.append(el("div", { class: "banner ok" }, "Конфликтов нет."));
  root.append(el("div", { class: "card" }, el("p", { class: "muted" }, "Конфликт: файл изменили на двух машинах одновременно. Ничего не потеряно: победившая версия лежит под обычным именем, вторая рядом, с пометкой sync-conflict. Оставьте нужную и удалите лишнюю."),
    el("table", {}, el("tr", {}, ["Файл", "Папка", "Изменён", "Размер"].map((h) => el("th", {}, h))),
      list.map((c) => el("tr", {}, el("td", { class: "mono" }, c.path, el("div", { class: "muted" }, "оригинал: " + c.original)), el("td", {}, c.folder), el("td", {}, ago(c.modified)), el("td", { class: "num" }, fmtBytes(c.size)))))));
}

async function viewPlan(root) {
  const plan = await api("/api/plan");
  if (!plan.length) return root.append(el("div", { class: "banner ok" }, "Менять нечего: настройки этой машины не старше настроек другой."));
  root.append(el("p", { class: "muted" }, "Что изменилось бы на этой машине по настройкам другой. Это только просмотр: применить можно командой «envsync settings apply», перед этим делается снимок для отката."));
  for (const p of plan) {
    root.append(el("div", { class: "card" }, el("h2", {}, p.module, el("span", { class: "muted" }, "от " + p.source)),
      p.error ? el("div", { class: "err" }, p.error) : el("ul", { class: "clean" }, p.changes.map((c) => el("li", { class: "mono" }, c)))));
  }
}

async function viewJournal(root) {
  const list = await api("/api/journal?n=200");
  if (!list.length) return root.append(el("div", { class: "card muted" }, "Журнал пуст."));
  root.append(el("div", { class: "card log" }, list.map((e) => el("div", { class: "row" },
    el("span", { class: "muted" }, new Date(e.time).toLocaleString("ru-RU")), el("span", {}, e.module), el("span", {}, pill(e.action === "error" || e.action === "trip" ? "bad" : e.action === "rollback" ? "warn" : "", e.action)),
    el("span", {}, (e.source ? "← " + e.source + ": " : "") + (e.detail || ""), e.changes && e.changes.length ? el("div", { class: "changes" }, e.changes.map((c) => el("div", {}, "• " + c))) : null)))));
}

async function viewNotSynced(root) {
  const sections = await api("/api/not-synced");
  root.append(el("p", { class: "muted" }, "Чтобы вы не думали, что что-то сломалось: вот что EnvSync не синхронизирует и почему."));
  for (const sec of sections) {
    root.append(el("div", { class: "card" }, el("h2", {}, sec.title),
      el("table", {}, el("tr", {}, ["Что", "Почему", "Как обойти"].map((h) => el("th", {}, h))),
        sec.items.map((i) => el("tr", {}, el("td", {}, i.what), el("td", {}, i.why), el("td", {}, i.workaround))))));
  }
}

const VIEWS = { status: viewStatus, decisions: viewDecisions, conflicts: viewConflicts, plan: viewPlan, journal: viewJournal, notsynced: viewNotSynced };

async function render() {
  const root = document.getElementById("view");
  const next = el("div", {});
  try {
    await VIEWS[current](next);
    document.getElementById("updated").textContent = "обновлено " + new Date().toLocaleTimeString("ru-RU");
  } catch (e) {
    next.append(el("div", { class: "banner" }, "Ошибка: " + e.message));
  }
  root.replaceChildren(...next.childNodes);
  renderTabs();
}

function renderTabs() {
  const trips = lastStatus ? (lastStatus.self.guard || []).length : 0;
  document.getElementById("tabs").replaceChildren(...TABS.map(([id, title]) =>
    el("button", { role: "tab", "aria-selected": String(id === current), onclick: () => { current = id; location.hash = id; render(); } },
      title, id === "decisions" && trips ? el("span", { class: "badge" }, trips) : null)));
  const paused = lastStatus && lastStatus.self.paused;
  const b = document.getElementById("pause");
  b.textContent = paused ? "Возобновить синхронизацию" : "Пауза всего";
  b.className = "btn" + (paused ? " primary" : "");
  b.onclick = async () => {
    try { await api("/api/pause?on=" + (paused ? "0" : "1"), true); } catch (e) { alert(e.message); }
    render();
  };
}

async function refreshStatusQuietly() {
  try { lastStatus = await api("/api/status"); renderTabs(); } catch (_) { /* покажется при следующем рендере */ }
}

render();
setInterval(() => (current === "status" || current === "decisions" ? render() : refreshStatusQuietly()), 5000);
