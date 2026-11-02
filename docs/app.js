"use strict";

const DATA_URL = "data/index.json";
const RAW_MODULE_BASE = "https://raw.githubusercontent.com/Xavier-0619/loon2surge/main/surge/";
const MODULE_PAGE_BASE = "https://github.com/Xavier-0619/loon2surge/blob/main/surge/";
const PAGE_SIZE = 24;

const state = {
  modules: [],
  query: "",
  category: "all",
  sort: "name",
  visible: PAGE_SIZE
};

const elements = {
  grid: document.querySelector("#module-grid"),
  search: document.querySelector("#search-input"),
  clearSearch: document.querySelector("#clear-search"),
  sort: document.querySelector("#sort-select"),
  categories: document.querySelector("#category-list"),
  count: document.querySelector("#results-count"),
  syncTime: document.querySelector("#sync-time"),
  empty: document.querySelector("#empty-state"),
  error: document.querySelector("#error-state"),
  errorMessage: document.querySelector("#error-message"),
  loadMore: document.querySelector("#load-more"),
  dialog: document.querySelector("#module-dialog"),
  dialogContent: document.querySelector("#dialog-content"),
  toast: document.querySelector("#toast")
};

let toastTimer;

function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, character => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  })[character]);
}

function normalize(value) {
  return String(value ?? "").trim().toLocaleLowerCase("zh-CN");
}

function encodedFileUrl(base, fileName) {
  return base + fileName.split("/").map(encodeURIComponent).join("/");
}

function moduleUrl(module) {
  return encodedFileUrl(RAW_MODULE_BASE, `${module.stem}.sgmodule`);
}

function installUrl(module) {
  return `surge:///install-module?url=${encodeURIComponent(moduleUrl(module))}`;
}

function sourcePageUrl(module) {
  return encodedFileUrl(MODULE_PAGE_BASE, `${module.stem}.sgmodule`);
}

function avatarText(name) {
  return Array.from(String(name || "模块").replace(/\s/g, "")).slice(0, 2).join("");
}

function avatarTone(module) {
  let hash = 0;
  for (const character of module.stem) hash = (hash * 31 + character.codePointAt(0)) >>> 0;
  return hash % 6;
}

function avatarMarkup(module, name) {
  const icon = /^https:\/\//i.test(String(module.icon || "").trim()) ? module.icon.trim() : "";
  return `<div class="module-avatar tone-${avatarTone(module)}" aria-hidden="true">${icon ? `<img class="module-icon" src="${escapeHTML(icon)}" alt="" loading="lazy" decoding="async" referrerpolicy="no-referrer">` : ""}<span class="avatar-fallback">${escapeHTML(avatarText(name))}</span></div>`;
}

// 含无法自动确认的转换内容，需人工核对，不在网页上架
function needsReview(module) {
  return Array.isArray(module.review) && module.review.length > 0;
}

function hasAdapted(module) {
  return Array.isArray(module.adapted) && module.adapted.length > 0;
}

function statusMarkup(module) {
  if (hasAdapted(module)) {
    return '<span class="card-status"><svg class="icon"><use href="#i-check"/></svg>已适配</span>';
  }
  return '<span class="card-status"><svg class="icon"><use href="#i-check"/></svg>可导入</span>';
}

function moduleCard(module, index) {
  const name = module.name || module.stem;
  const authors = (module.authors || []).join(" / ") || "可莉插件中心";
  const description = module.desc || "暂无模块描述，点击查看模块详情与原始文件。";
  const tags = (module.tags || []).slice(0, 2);
  const extraTags = Math.max(0, (module.tags || []).length - tags.length);
  const date = module.date ? module.date.slice(0, 10) : "未知日期";

  return `
    <article class="module-card" style="animation-delay:${Math.min(index % PAGE_SIZE, 10) * 24}ms">
      <div class="card-top">
        ${avatarMarkup(module, name)}
        <div class="card-title-wrap">
          <button class="card-title-button" type="button" data-action="details" data-id="${escapeHTML(module.stem)}" aria-label="查看 ${escapeHTML(name)} 的模块详情">
            <h3 class="card-title" title="${escapeHTML(name)}">${escapeHTML(name)}</h3>
          </button>
          <span class="card-author" title="${escapeHTML(authors)}">by ${escapeHTML(authors)}</span>
        </div>
        ${statusMarkup(module)}
      </div>
      <p class="card-desc">${escapeHTML(description)}</p>
      <div class="card-tags">
        ${tags.map(tag => `<span class="card-tag">${escapeHTML(tag)}</span>`).join("")}
        ${extraTags ? `<span class="card-tag">+${extraTags}</span>` : ""}
      </div>
      <div class="card-footer">
        <span class="card-date">${escapeHTML(date)}</span>
        <button class="icon-button card-action" type="button" data-action="copy" data-id="${escapeHTML(module.stem)}" aria-label="复制 ${escapeHTML(name)} 的模块链接" title="复制模块链接"><svg class="icon"><use href="#i-copy"/></svg></button>
        <a class="install-button" href="${escapeHTML(installUrl(module))}" data-install="${escapeHTML(name)}" aria-label="将 ${escapeHTML(name)} 导入 Surge"><svg class="icon"><use href="#i-download"/></svg>导入 Surge</a>
      </div>
    </article>`;
}

function filteredModules() {
  const query = normalize(state.query);
  return state.modules
    .filter(module => state.category === "all" || (module.tags || []).includes(state.category))
    .filter(module => !query || module.searchText.includes(query))
    .sort((a, b) => {
      if (state.sort === "updated") {
        const byDate = String(b.date || "").localeCompare(String(a.date || ""));
        if (byDate) return byDate;
      }
      return (a.name || a.stem).localeCompare(b.name || b.stem, "zh-CN", { numeric: true });
    });
}

function renderModules() {
  const filtered = filteredModules();
  const visibleModules = filtered.slice(0, state.visible);
  const remaining = Math.max(0, filtered.length - visibleModules.length);

  elements.grid.innerHTML = visibleModules.map(moduleCard).join("");
  elements.grid.setAttribute("aria-busy", "false");
  elements.grid.hidden = filtered.length === 0;
  elements.empty.hidden = filtered.length !== 0;
  elements.count.innerHTML = state.query || state.category !== "all"
    ? `找到 <strong>${filtered.length}</strong> 个匹配模块 <span>· 全部 ${state.modules.length} 个</span>`
    : `共 <strong>${filtered.length}</strong> 个可导入模块`;
  elements.clearSearch.hidden = !state.query;
  elements.loadMore.hidden = remaining === 0;
  if (remaining) elements.loadMore.innerHTML = `加载更多模块（剩余 ${remaining} 个）<svg class="icon"><use href="#i-chevron"/></svg>`;
}

function renderCategories() {
  const counts = new Map();
  for (const module of state.modules) {
    for (const tag of new Set(module.tags || [])) counts.set(tag, (counts.get(tag) || 0) + 1);
  }
  const categories = [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0], "zh-CN"));
  elements.categories.innerHTML = [
    `<button class="category-chip active" type="button" data-category="all" aria-pressed="true">全部模块 <span>${state.modules.length}</span></button>`,
    ...categories.map(([tag, count]) => `<button class="category-chip" type="button" data-category="${escapeHTML(tag)}" aria-pressed="false">${escapeHTML(tag)} <span>${count}</span></button>`)
  ].join("");
  document.querySelector("#stat-categories").textContent = counts.size;
}

function setCategory(category) {
  state.category = category;
  state.visible = PAGE_SIZE;
  for (const button of elements.categories.querySelectorAll("[data-category]")) {
    const active = button.dataset.category === category;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  }
  renderModules();
}

function resetFilters() {
  state.query = "";
  state.sort = "name";
  elements.search.value = "";
  elements.sort.value = "name";
  setCategory("all");
  elements.search.focus();
}

function showToast(message) {
  clearTimeout(toastTimer);
  elements.toast.textContent = message;
  elements.toast.classList.add("show");
  toastTimer = setTimeout(() => elements.toast.classList.remove("show"), 2400);
}

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.style.cssText = "position:fixed;opacity:0;pointer-events:none";
  document.body.append(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("copy failed");
}

async function copyModuleLink(module) {
  try {
    await copyText(moduleUrl(module));
    showToast("模块链接已复制，可在 Surge 中手动添加");
  } catch {
    showToast("复制失败，请在模块详情中查看原始文件");
  }
}

function diagnosticList(title, items, className = "") {
  if (!Array.isArray(items) || !items.length) return "";
  return `<section class="diagnostic-section"><h3><svg class="icon"><use href="#i-info"/></svg>${escapeHTML(title)}</h3><ul class="diagnostic-list ${className}">${items.map(item => `<li>${escapeHTML(item)}</li>`).join("")}</ul></section>`;
}

function openDetails(module) {
  const name = module.name || module.stem;
  const authors = (module.authors || []).join(" / ") || "可莉插件中心";
  elements.dialogContent.innerHTML = `
    <div class="dialog-header">
      ${avatarMarkup(module, name)}
      <div><h2 id="dialog-title">${escapeHTML(name)}</h2><p>by ${escapeHTML(authors)}</p></div>
    </div>
    <p class="dialog-description">${escapeHTML(module.desc || "暂无模块描述。")}</p>
    <div class="dialog-tags">${(module.tags || []).map(tag => `<span class="card-tag">${escapeHTML(tag)}</span>`).join("")}${statusMarkup(module)}</div>
    <div class="dialog-actions">
      <a class="button button-primary" href="${escapeHTML(installUrl(module))}" data-install="${escapeHTML(name)}"><svg class="icon"><use href="#i-download"/></svg>一键导入 Surge</a>
      <button class="button button-quiet" type="button" data-dialog-copy="${escapeHTML(module.stem)}"><svg class="icon"><use href="#i-copy"/></svg>复制模块链接</button>
    </div>
    <dl class="dialog-meta">
      <div><dt>上游更新时间</dt><dd>${escapeHTML(module.date || "未知")}</dd></div>
      <div><dt>转换器版本</dt><dd>v${escapeHTML(module.converter || "未知")}</dd></div>
      <div><dt>模块文件</dt><dd>${escapeHTML(module.stem)}.sgmodule</dd></div>
      <div><dt>模块作者</dt><dd>${escapeHTML(authors)}</dd></div>
    </dl>
    ${diagnosticList("已自动适配", module.adapted)}
    ${diagnosticList("转换记录", module.notes)}
    <div class="dialog-source"><a href="${escapeHTML(sourcePageUrl(module))}" target="_blank" rel="noreferrer">在 GitHub 查看原始模块 <svg class="icon"><use href="#i-external"/></svg></a></div>`;
  if (!elements.dialog.open) elements.dialog.showModal();
}

function findModule(id) {
  return state.modules.find(module => module.stem === id);
}

function formatSyncTime(value) {
  if (!value) return "";
  const date = new Date(String(value).replace(" UTC", "Z").replace(" ", "T"));
  if (Number.isNaN(date.getTime())) return `数据同步于 ${value}`;
  const formatted = new Intl.DateTimeFormat("zh-CN", {
    timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", hour12: false
  }).format(date);
  return `数据同步于 ${formatted}（北京时间）`;
}

function setLoading(loading) {
  elements.search.disabled = loading;
  elements.sort.disabled = loading;
  elements.grid.setAttribute("aria-busy", String(loading));
}

async function loadModules() {
  setLoading(true);
  elements.error.hidden = true;
  elements.empty.hidden = true;
  elements.grid.hidden = false;
  elements.loadMore.hidden = true;
  elements.grid.innerHTML = '<div class="skeleton-card"></div><div class="skeleton-card"></div><div class="skeleton-card"></div><div class="skeleton-card"></div><div class="skeleton-card"></div><div class="skeleton-card"></div>';
  elements.count.textContent = "正在准备模块库…";
  elements.syncTime.textContent = "";

  try {
    const response = await fetch(DATA_URL, { cache: "no-store" });
    if (!response.ok) throw new Error(`数据请求返回 ${response.status}`);
    const data = await response.json();
    if (!Array.isArray(data.plugins)) throw new Error("索引数据格式不正确");

    state.modules = data.plugins
      .filter(module => module && module.status === "ok" && module.stem)
      // 需要人工确认的模块不上架：部分内容无法自动验证，导入后行为不确定
      .filter(module => !needsReview(module))
      .map(module => ({
        ...module,
        searchText: normalize([
          module.name, module.stem, module.desc,
          (module.authors || []).join(" "), (module.tags || []).join(" ")
        ].join(" "))
      }));
    state.visible = PAGE_SIZE;
    document.querySelector("#stat-modules").textContent = state.modules.length;
    renderCategories();
    renderModules();
    elements.syncTime.textContent = formatSyncTime(data.synced_at);
  } catch (error) {
    elements.grid.hidden = true;
    elements.grid.innerHTML = "";
    elements.count.textContent = "模块加载失败";
    elements.errorMessage.textContent = `${error.message}。请检查网络连接后重试。`;
    elements.error.hidden = false;
  } finally {
    setLoading(false);
  }
}

elements.search.addEventListener("input", event => {
  state.query = event.target.value;
  state.visible = PAGE_SIZE;
  renderModules();
});

elements.clearSearch.addEventListener("click", () => {
  state.query = "";
  state.visible = PAGE_SIZE;
  elements.search.value = "";
  renderModules();
  elements.search.focus();
});

elements.sort.addEventListener("change", event => {
  state.sort = event.target.value;
  state.visible = PAGE_SIZE;
  renderModules();
});

elements.categories.addEventListener("click", event => {
  const button = event.target.closest("[data-category]");
  if (button) setCategory(button.dataset.category);
});

elements.loadMore.addEventListener("click", () => {
  state.visible += PAGE_SIZE;
  renderModules();
});

document.querySelector("#reset-filters").addEventListener("click", resetFilters);
document.querySelector("#retry-load").addEventListener("click", loadModules);
document.querySelector("#dialog-close").addEventListener("click", () => elements.dialog.close());

elements.dialog.addEventListener("click", event => {
  if (event.target === elements.dialog) elements.dialog.close();
});

document.addEventListener("click", event => {
  const action = event.target.closest("[data-action]");
  if (action) {
    const module = findModule(action.dataset.id);
    if (!module) return;
    if (action.dataset.action === "details") openDetails(module);
    if (action.dataset.action === "copy") copyModuleLink(module);
  }

  const dialogCopy = event.target.closest("[data-dialog-copy]");
  if (dialogCopy) {
    const module = findModule(dialogCopy.dataset.dialogCopy);
    if (module) copyModuleLink(module);
  }

  const install = event.target.closest("[data-install]");
  if (install) showToast(`正在唤起 Surge 导入「${install.dataset.install}」…`);
});

document.addEventListener("error", event => {
  if (event.target instanceof HTMLImageElement && event.target.classList.contains("module-icon")) {
    event.target.remove();
  }
}, true);

document.addEventListener("keydown", event => {
  const tagName = document.activeElement?.tagName;
  if (event.key === "/" && !["INPUT", "TEXTAREA", "SELECT"].includes(tagName) && !elements.dialog.open) {
    event.preventDefault();
    elements.search.focus();
  }
  if (event.key === "Escape" && document.activeElement === elements.search && state.query) {
    state.query = "";
    state.visible = PAGE_SIZE;
    elements.search.value = "";
    renderModules();
  }
});

loadModules();
