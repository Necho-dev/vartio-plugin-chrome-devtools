// @ts-check
// Chrome DevTools plugin UI, mounted by the probe-hub page (plugins.js):
// endpoint discovery, browser launch, target management and a CDP console.
import { esc } from "/modules/core.js";
import { icon } from "/modules/icons.js";

/**
 * @typedef {{ ip: string, port: number, pid?: number, process?: string, browser: string, protocolVersion?: string, userAgent?: string, webSocketDebuggerUrl?: string, localWebSocketUrl?: string }} Endpoint
 * @typedef {{ key: string, name: string, path: string }} Installed
 * @typedef {{ id: string, type: string, title: string, url: string, webSocketDebuggerUrl?: string, devtoolsFrontendUrl?: string, attached?: boolean }} Target
 * @typedef {{ agent: string, embedded: boolean, base: string, params: URLSearchParams, api: (p: string, o?: any) => Promise<any>, open: (n: string, q?: Record<string, string>) => void }} Ctx
 */

const METHODS = [
  "Browser.getVersion", "Browser.close", "Target.getTargets", "Target.createTarget", "Target.closeTarget", "Target.activateTarget",
  "Page.enable", "Page.navigate", "Page.reload", "Page.captureScreenshot", "Page.printToPDF", "Page.getLayoutMetrics", "Page.bringToFront",
  "Runtime.enable", "Runtime.evaluate", "Runtime.getProperties", "DOM.getDocument", "DOM.querySelector", "DOM.getOuterHTML",
  "Network.enable", "Network.disable", "Network.getCookies", "Network.setUserAgentOverride", "Network.clearBrowserCache",
  "Emulation.setDeviceMetricsOverride", "Emulation.clearDeviceMetricsOverride", "Input.dispatchMouseEvent", "Input.insertText",
  "Log.enable", "Performance.enable", "Performance.getMetrics", "Storage.clearDataForOrigin",
];

/** @type {[label: string, method: string, params: any, scope: "page" | "browser" | "any"][]} */
const QUICK = [
  ["标题", "Runtime.evaluate", { expression: "document.title", returnByValue: true }, "page"],
  ["打开网址", "Page.navigate", { url: "https://example.com" }, "page"],
  ["截图", "Page.captureScreenshot", { format: "png" }, "page"],
  ["刷新", "Page.reload", {}, "page"],
  ["版本", "Browser.getVersion", {}, "any"],
  ["全部目标", "Target.getTargets", {}, "browser"],
  ["新标签页", "Target.createTarget", { url: "about:blank" }, "browser"],
];

/** @param {HTMLElement} el @param {Ctx} ctx */
export async function mount(el, ctx) {
  const A = `/agents/${encodeURIComponent(ctx.agent)}`;
  /** @type {Endpoint[]} */
  let running = [];
  /** @type {Installed[]} */
  let installed = [];
  /** @type {Endpoint | null} */
  let sel = null;
  /** @type {WebSocket | null} */
  let ws = null;
  let alive = true;

  /** @param {string} r @returns {any} */
  const $ = (r) => el.querySelector(`[data-r="${r}"]`);
  const ago = () => new Date().toLocaleTimeString([], { hour12: false });

  // ---------- endpoints view ----------
  const home = () => {
    closeWS();
    el.innerHTML = `
      <div class="pl-bar">
        <button class="btn sm" data-r="refresh">${icon("refresh-cw")}刷新</button>
        <label class="pl-meta"><input type="checkbox" data-r="all"> 探测全部回环端口</label>
        <span class="pl-meta" data-r="st"></span><span class="pl-grow"></span>
        <input class="input" data-r="port" placeholder="端口，如 9222" style="width:130px" inputmode="numeric">
        <button class="btn sm" data-r="go">连接端口</button>
      </div>
      <div class="pl-sec"><h4>调试端点 <span class="faint">已开启远程调试（--remote-debugging-port）的浏览器，含比特浏览器打开的窗口</span></h4><div data-r="running"></div></div>
      <div class="pl-sec" data-r="targetsSec" hidden></div>
      <div class="pl-sec"><h4>已安装浏览器 <span class="faint">以独立用户目录启动并开启调试端口</span></h4><div data-r="installed"></div></div>`;
    $("refresh").addEventListener("click", load);
    $("all").addEventListener("change", load);
    const go = () => {
      const n = Number($("port").value.trim());
      if (n > 0 && n < 65536) openPort(n);
    };
    $("go").addEventListener("click", go);
    $("port").addEventListener("keydown", (/** @type {KeyboardEvent} */ e) => { if (e.key === "Enter") go(); });
    drawRunning();
    drawInstalled();
    if (sel) drawTargets(sel);
  };

  const load = async () => {
    const st = $("st");
    if (st) { st.className = "pl-meta"; st.textContent = "探测中…"; }
    try {
      const r = await ctx.api(`${A}/browsers${$("all")?.checked ? "?all=1" : ""}`);
      if (!alive) return;
      running = r.running;
      installed = r.installed;
      if ($("st")) {
        $("st").className = "pl-meta" + (r.errors?.length ? " bad" : "");
        $("st").textContent = `${ago()} · 探测 ${r.probed} 个监听端口，发现 ${running.length} 个调试端点` + (r.errors?.length ? " · " + r.errors.join("；") : "");
      }
      drawRunning();
      drawInstalled();
      if (sel && !running.some((e) => e.port === sel?.port)) { sel = null; const t = $("targetsSec"); if (t) t.hidden = true; }
    } catch (e) {
      if ($("st")) { $("st").className = "pl-meta bad"; $("st").textContent = e.message; }
    }
  };

  const drawRunning = () => {
    const box = $("running");
    if (!box) return;
    if (!running.length) { box.innerHTML = `<div class="pl-meta">没有发现调试端点。可在下方启动浏览器，或在比特浏览器插件里打开窗口后按端口连接。</div>`; return; }
    box.innerHTML = `<table class="pl-table"><tr><th>端口</th><th>浏览器</th><th>进程</th><th></th></tr>${running.map((e) => `<tr class="${sel && sel.port === e.port ? "on" : ""}">
      <td class="mono">${esc(e.ip)}:${e.port}</td><td>${esc(e.browser)}${e.wsOnly ? '<span class="pl-pill info" title="在 chrome://inspect 开启的远程调试：没有 /json 接口，每次连接都要在该浏览器里点「允许」">仅 WebSocket</span>' : ""}</td><td class="clip faint">${esc(e.process || "—")}${e.pid ? ` · ${e.pid}` : ""}</td>
      <td class="acts">${e.wsOnly ? "" : `<button class="btn sm" data-port="${e.port}" data-act="targets">${icon("list")}页面</button>`}<button class="btn sm" data-port="${e.port}" data-act="browser">${icon("square-terminal")}浏览器会话</button><button class="btn sm" data-port="${e.port}" data-act="copy" title="复制经 Hub 的 browser WebSocket 地址（需 Authorization 头）">${icon("copy")}</button>${e.wsOnly ? "" : `<button class="btn sm danger" data-port="${e.port}" data-act="quit" title="发送 Browser.close，浏览器正常退出">关闭</button>`}</td></tr>`).join("")}</table>`;
    box.querySelectorAll("[data-act]").forEach((/** @type {HTMLElement} */ b) => b.addEventListener("click", async () => {
      const e = running.find((x) => x.port === Number(b.dataset.port));
      if (!e) return;
      if (b.dataset.act === "targets") { sel = e; drawRunning(); drawTargets(e); }
      if (b.dataset.act === "browser") consoleView(e, null);
      if (b.dataset.act === "quit") {
        if (!confirm(`关闭 ${e.browser}（端口 ${e.port}${e.pid ? `，pid ${e.pid}` : ""}）？浏览器的所有窗口都会关闭。`)) return;
        b.setAttribute("disabled", "");
        try {
          await ctx.api(`${A}/ports/${e.port}/close`, { method: "POST" });
          if (sel?.port === e.port) { sel = null; const t = $("targetsSec"); if (t) t.hidden = true; }
          await new Promise((ok) => setTimeout(ok, 800));
          await load();
        } catch (err) { b.removeAttribute("disabled"); alert(err.message); }
      }
      if (b.dataset.act === "copy" && e.webSocketDebuggerUrl) {
        try { await navigator.clipboard.writeText(e.webSocketDebuggerUrl); b.innerHTML = icon("check"); } catch { prompt("browser WebSocket", e.webSocketDebuggerUrl); }
      }
    }));
  };

  const drawInstalled = () => {
    const box = $("installed");
    if (!box) return;
    if (!installed.length) { box.innerHTML = `<div class="pl-meta">没有找到已知位置的 Chrome / Edge / Chromium / Brave。</div>`; return; }
    box.innerHTML = `<table class="pl-table"><tr><th>浏览器</th><th>路径</th><th></th></tr>${installed.map((b) => `<tr><td>${esc(b.name)}</td><td class="clip mono faint" title="${esc(b.path)}">${esc(b.path)}</td>
      <td class="acts"><button class="btn sm" data-k="${esc(b.key)}">${icon("play")}启动</button></td></tr>`).join("")}</table>
      <div data-r="launch"></div>`;
    box.querySelectorAll("[data-k]").forEach((/** @type {HTMLElement} */ b) => b.addEventListener("click", () => launchForm(b.dataset.k || "")));
  };

  /** @param {string} key */
  const launchForm = (key) => {
    const b = installed.find((x) => x.key === key);
    if (!b) return;
    $("launch").innerHTML = `<div class="card" style="padding:12px;margin-top:10px">
      <div class="pl-bar"><b>启动 ${esc(b.name)}</b><span class="pl-meta">进程以探针所在用户运行；Windows 服务方式运行的探针看不到桌面窗口</span></div>
      <div class="pl-bar">
        <input class="input" data-r="lPort" placeholder="调试端口（留空自动）" style="width:150px" inputmode="numeric">
        <input class="input pl-grow" data-r="lUrl" placeholder="打开网址（可空）">
        <label class="pl-meta"><input type="checkbox" data-r="lHeadless"> 无头模式</label>
      </div>
      <div class="pl-bar">
        <input class="input pl-grow" data-r="lDir" placeholder="用户目录，默认 ~/.probe-cdp/${esc(key)}（同一目录只能有一个实例）">
        <input class="input pl-grow" data-r="lArgs" placeholder="附加参数，空格分隔，如 --window-size=1280,800">
      </div>
      <div class="pl-bar"><button class="btn primary sm" data-r="lGo">${icon("play")}启动并连接</button><button class="btn sm" data-r="lCancel">取消</button><span class="pl-meta" data-r="lSt"></span></div></div>`;
    $("lCancel").addEventListener("click", () => { $("launch").innerHTML = ""; });
    $("lGo").addEventListener("click", async () => {
      const st = $("lSt");
      $("lGo").disabled = true;
      st.className = "pl-meta";
      st.textContent = "启动中，等待调试端口就绪…";
      try {
        const r = await ctx.api(`${A}/launch`, { method: "POST", json: {
          browser: key, port: Number($("lPort").value) || 0, url: $("lUrl").value.trim(), headless: $("lHeadless").checked,
          userDataDir: $("lDir").value.trim(), args: $("lArgs").value.trim() ? $("lArgs").value.trim().split(/\s+/) : [],
        } });
        st.className = "pl-meta ok";
        st.textContent = `已启动 pid ${r.pid}，端口 ${r.endpoint.port} · ${r.endpoint.browser}`;
        await load();
        const e = running.find((x) => x.port === r.endpoint.port) || r.endpoint;
        sel = e;
        drawRunning();
        drawTargets(e);
      } catch (e) {
        st.className = "pl-meta bad";
        st.textContent = e.message;
      } finally {
        if ($("lGo")) $("lGo").disabled = false;
      }
    });
  };

  /** @param {number} port */
  const openPort = async (port) => {
    try {
      const v = await ctx.api(`${A}/ports/${port}/json/version`);
      /** @type {Endpoint} */
      const e = running.find((x) => x.port === port) || { ip: "127.0.0.1", port, browser: v.Browser, protocolVersion: v["Protocol-Version"], userAgent: v["User-Agent"], webSocketDebuggerUrl: v.webSocketDebuggerUrl };
      if (!running.some((x) => x.port === port)) running = [...running, e].sort((a, b) => a.port - b.port);
      sel = e;
      drawRunning();
      drawTargets(e);
    } catch (err) {
      if ($("st")) { $("st").className = "pl-meta bad"; $("st").textContent = `端口 ${port}：${err.message}`; }
    }
  };

  // ---------- targets ----------
  /** @param {Endpoint} e */
  const drawTargets = async (e) => {
    const sec = $("targetsSec");
    if (!sec) return;
    sec.hidden = false;
    sec.innerHTML = `<h4>页面与目标 <span class="faint">${esc(e.browser)} · 端口 ${e.port}</span></h4>
      <div class="pl-bar"><input class="input pl-grow" data-r="newUrl" placeholder="新标签页网址" value="about:blank"><button class="btn sm" data-r="newTab">${icon("plus")}新建标签页</button><button class="btn sm" data-r="reloadT">${icon("refresh-cw")}刷新</button></div>
      <div data-r="targets"><div class="pl-meta">加载中…</div></div>`;
    $("reloadT").addEventListener("click", () => drawTargets(e));
    $("newTab").addEventListener("click", async () => {
      try {
        await ctx.api(`${A}/ports/${e.port}/json/new?${encodeURIComponent($("newUrl").value.trim() || "about:blank")}`, { method: "PUT" });
        drawTargets(e);
      } catch (err) { alert(err.message); }
    });
    /** @type {Target[]} */
    let list;
    try { list = await ctx.api(`${A}/ports/${e.port}/json/list`); } catch (err) {
      if ($("targets")) $("targets").innerHTML = `<div class="pl-meta bad">${esc(err.message)}</div>`;
      return;
    }
    const box = $("targets");
    if (!box) return;
    if (!list.length) { box.innerHTML = `<div class="pl-meta">没有目标</div>`; return; }
    box.innerHTML = `<table class="pl-table"><tr><th>类型</th><th>标题 / 网址</th><th></th></tr>${list.map((t, i) => `<tr>
      <td><span class="pl-pill ${t.type === "page" ? "ok" : ""}">${esc(t.type)}</span></td>
      <td class="clip" title="${esc(t.url)}"><div>${esc(t.title || "（无标题）")}</div><div class="faint mono" style="font-size:11.5px">${esc(t.url)}</div></td>
      <td class="acts">${t.webSocketDebuggerUrl ? `<button class="btn sm primary" data-i="${i}" data-act="debug">${icon("square-terminal")}调试</button>` : '<span class="pl-pill">已被占用</span>'}
        ${t.type === "page" ? `<button class="btn sm" data-i="${i}" data-act="activate">激活</button>` : ""}
        <button class="btn sm danger" data-i="${i}" data-act="close">关闭</button></td></tr>`).join("")}</table>`;
    box.querySelectorAll("[data-act]").forEach((/** @type {HTMLElement} */ b) => b.addEventListener("click", async () => {
      const t = list[Number(b.dataset.i)];
      if (b.dataset.act === "debug") return consoleView(e, t);
      try {
        if (b.dataset.act === "close" && !confirm(`关闭「${t.title || t.url}」？`)) return;
        await ctx.api(`${A}/ports/${e.port}/json/${b.dataset.act}/${encodeURIComponent(t.id)}`);
        drawTargets(e);
      } catch (err) { alert(err.message); }
    }));
  };

  // ---------- console ----------
  const closeWS = () => { if (ws) { ws.onclose = null; ws.close(); } ws = null; };

  /** @param {Endpoint} e @param {Target | null} t */
  const consoleView = (e, t) => {
    const scope = t ? "page" : "browser";
    const wsURL = t ? t.webSocketDebuggerUrl : e.webSocketDebuggerUrl;
    el.innerHTML = `
      <div class="pl-bar"><button class="btn sm" data-r="back">${icon("arrow-left")}返回</button>
        <b>${esc(t ? t.title || t.url : e.browser + " · 浏览器会话")}</b><span class="pl-meta" data-r="cst">连接中…</span><span class="pl-grow"></span>
        <label class="pl-meta"><input type="checkbox" data-r="events" checked> 显示事件</label><button class="btn sm" data-r="clear">清空</button></div>
      <div class="pl-bar">${QUICK.filter((q) => q[3] === "any" || q[3] === scope).map((q, i) => `<button class="btn sm" data-q="${i}">${esc(q[0])}</button>`).join("")}</div>
      <div class="pl-split" style="min-height:0">
        <div>
          <input class="input" data-r="method" list="cdpMethods" placeholder="方法，如 Runtime.evaluate" style="width:100%;margin-bottom:8px" spellcheck="false">
          <datalist id="cdpMethods">${METHODS.map((m) => `<option value="${m}">`).join("")}</datalist>
          <textarea class="input" data-r="params" rows="10" spellcheck="false" style="width:100%" placeholder="params（JSON）">{}</textarea>
          <div class="pl-bar" style="margin-top:8px"><button class="btn primary" data-r="send" disabled>${icon("send")}发送</button><span class="pl-meta">Ctrl / ⌘ + Enter</span></div>
          <div data-r="shot"></div>
          <div class="pl-meta" style="margin-top:8px;word-break:break-all">外部工具可直接连：<code class="mono">${esc(wsURL || "")}</code>（需 Authorization: Bearer 管理令牌）</div>
        </div>
        <div class="pl-list" data-r="log" style="max-height:65vh;cursor:default"></div>
      </div>`;
    const filtered = QUICK.filter((q) => q[3] === "any" || q[3] === scope);
    $("back").addEventListener("click", () => { home(); load(); });
    $("clear").addEventListener("click", () => { $("log").innerHTML = ""; });
    el.querySelectorAll("[data-q]").forEach((/** @type {HTMLElement} */ b) => b.addEventListener("click", () => {
      const q = filtered[Number(b.dataset.q)];
      $("method").value = q[1];
      $("params").value = JSON.stringify(q[2], null, 2);
      send();
    }));
    let seq = 0;
    /** @type {Map<number, { method: string, t0: number }>} */
    const pending = new Map();
    /** @param {"out" | "in" | "ev" | "sys"} dir @param {string} head @param {any} [body] @param {boolean} [bad] */
    const log = (dir, head, body, bad) => {
      const box = $("log");
      if (!box) return;
      if (dir === "ev" && !$("events").checked) return;
      const d = document.createElement("div");
      const arrow = { out: "→", in: "←", ev: "◆", sys: "·" }[dir];
      const text = body === undefined ? "" : JSON.stringify(body, (k, v) => (typeof v === "string" && v.length > 300 ? v.slice(0, 300) + `…（共 ${v.length} 字符）` : v), 2);
      d.innerHTML = `<div class="${bad ? "pl-meta bad" : dir === "ev" ? "faint" : ""}"><span class="mono">${arrow}</span> ${esc(head)} <span class="faint" style="float:right">${ago()}</span></div>${text ? `<details${dir === "in" && text.length < 4000 ? " open" : ""}><summary class="faint">详情</summary><pre class="pl-pre" style="margin-top:4px;max-height:280px">${esc(text)}</pre></details>` : ""}`;
      box.prepend(d);
      while (box.children.length > 400) box.lastChild?.remove();
    };
    const send = () => {
      const method = $("method").value.trim();
      if (!method || !ws || ws.readyState !== WebSocket.OPEN) return;
      let params;
      try { params = JSON.parse($("params").value || "{}"); } catch (err) { log("sys", "params 不是合法 JSON：" + err.message, undefined, true); return; }
      const id = ++seq;
      pending.set(id, { method, t0: performance.now() });
      ws.send(JSON.stringify({ id, method, params }));
      log("out", `#${id} ${method}`, params);
    };
    $("send").addEventListener("click", send);
    $("params").addEventListener("keydown", (/** @type {KeyboardEvent} */ ev) => { if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) { ev.preventDefault(); send(); } });
    $("method").addEventListener("keydown", (/** @type {KeyboardEvent} */ ev) => { if (ev.key === "Enter") { ev.preventDefault(); send(); } });

    (async () => {
      if (!wsURL) { $("cst").className = "pl-meta bad"; $("cst").textContent = "目标没有 WebSocket 地址（可能已被其他 DevTools 客户端占用）"; return; }
      try {
        const path = wsURL.slice(wsURL.indexOf("/devtools/"));
        const tk = await ctx.api(`${A}/ports/${e.port}/ticket`, { method: "POST", json: { path } });
        if (!alive) return;
        closeWS();
        const sock = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}${tk.ws}`);
        ws = sock;
        sock.onopen = () => {
          $("cst").className = "pl-meta ok";
          $("cst").textContent = "已连接";
          $("send").disabled = false;
          log("sys", `已连接 ${path}`);
        };
        sock.onmessage = (m) => {
          /** @type {any} */
          let msg;
          try { msg = JSON.parse(m.data); } catch { log("in", "非 JSON 消息", String(m.data).slice(0, 2000)); return; }
          if (msg.id != null) {
            const p = pending.get(msg.id);
            pending.delete(msg.id);
            const ms = p ? Math.round(performance.now() - p.t0) : 0;
            log("in", `#${msg.id} ${p ? p.method : ""} · ${ms} ms${msg.error ? " · 错误" : ""}`, msg.error || msg.result, !!msg.error);
            if (p && p.method === "Page.captureScreenshot" && msg.result && msg.result.data) {
              $("shot").innerHTML = `<div class="pl-sec" style="margin-top:10px"><h4>${icon("camera")}截图</h4><img alt="截图" style="max-width:100%;border:1px solid var(--border);border-radius:8px" src="data:image/png;base64,${msg.result.data}"></div>`;
            }
          } else {
            log("ev", msg.method || "event", msg.params);
          }
        };
        sock.onclose = (ev) => {
          if (ws !== sock) return;
          ws = null;
          if ($("cst")) { $("cst").className = "pl-meta bad"; $("cst").textContent = `已断开（${ev.code}${ev.reason ? " " + ev.reason : ""}）`; }
          if ($("send")) $("send").disabled = true;
          log("sys", "连接已关闭", undefined, true);
        };
      } catch (err) {
        $("cst").className = "pl-meta bad";
        $("cst").textContent = err.message;
      }
    })();
  };

  home();
  await load();
  const want = Number(ctx.params.get("port"));
  if (want > 0) openPort(want);
  return () => { alive = false; closeWS(); };
}
