// 应用层：状态、筛选与事件绑定。HTML 片段一律来自 render.js，这里只负责「什么时候渲染什么」。
import {
  PAGE_SIZE,
  normalize,
  moduleUrl,
  cardMarkup,
  categoriesMarkup,
  resultsMarkup,
  detailsMarkup,
  formatSyncTime,
} from "./render.js";

const DATA_URL = "data/index.json";

const state = {
  modules: [],
  query: "",
  category: "all",
  sort: "name",
  visible: PAGE_SIZE,
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
  toast: document.querySelector("#toast"),
  statModules: document.querySelector("#stat-modules"),
  statCategories: document.querySelector("#stat-categories"),
};

let toastTimer = 0;

/** 含无法自动确认的转换内容，需人工核对，因此不在网页上架。 */
function needsReview(module) {
  return Array.isArray(module.review) && module.review.length > 0;
}

function findModule(stem) {
  return state.modules.find((module) => module.stem === stem);
}

function filteredModules() {
  const query = normalize(state.query);
  const matched = state.modules.filter((module) => {
    if (state.category !== "all" && !(module.tags ?? []).includes(state.category)) return false;
    return !query || module.searchText.includes(query);
  });
  return matched.sort((a, b) => {
    if (state.sort === "updated") {
      const byDate = String(b.date ?? "").localeCompare(String(a.date ?? ""));
      if (byDate) return byDate;
    }
    return (a.name || a.stem).localeCompare(b.name || b.stem, "zh-CN", { numeric: true });
  });
}

function renderModules() {
  const matched = filteredModules();
  const visible = matched.slice(0, state.visible);
  const remaining = matched.length - visible.length;

  elements.grid.innerHTML = visible.map(cardMarkup).join("");
  elements.grid.setAttribute("aria-busy", "false");
  elements.grid.hidden = matched.length === 0;
  elements.empty.hidden = matched.length !== 0;

  const filtering = Boolean(state.query) || state.category !== "all";
  elements.count.innerHTML = resultsMarkup(matched.length, state.modules.length, filtering);
  elements.clearSearch.hidden = !state.query;
  elements.loadMore.hidden = remaining === 0;
  if (remaining > 0) {
    elements.loadMore.innerHTML = `加载更多模块（剩余 ${remaining} 个）<svg class="icon"><use href="#i-chevron"/></svg>`;
  }
}

function renderCategories() {
  const counts = new Map();
  for (const module of state.modules) {
    for (const tag of new Set(module.tags ?? [])) counts.set(tag, (counts.get(tag) ?? 0) + 1);
  }
  const categories = [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0], "zh-CN"));
  elements.categories.innerHTML = categoriesMarkup(categories, state.modules.length);
  elements.statCategories.textContent = counts.size;
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
  state.visible = PAGE_SIZE;
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
  // 非安全上下文（http 或 file:）没有 Clipboard API，退回旧办法。
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

function openDetails(module) {
  elements.dialogContent.innerHTML = detailsMarkup(module);
  if (!elements.dialog.open) elements.dialog.showModal();
}

function setLoading(loading) {
  elements.search.disabled = loading;
  elements.sort.disabled = loading;
  elements.grid.setAttribute("aria-busy", String(loading));
}

/** 索引里既可能有转换失败的条目，也可能有等待人工核对的模块，两者都不该出现在网页上。 */
function publishable(modules) {
  return modules
    .filter((module) => module && module.status === "ok" && module.stem)
    .filter((module) => !needsReview(module))
    .map((module) => ({
      ...module,
      searchText: normalize(
        [module.name, module.stem, module.desc, (module.authors ?? []).join(" "), (module.tags ?? []).join(" ")].join(" "),
      ),
    }));
}

async function loadModules() {
  setLoading(true);
  elements.error.hidden = true;
  elements.empty.hidden = true;
  elements.grid.hidden = false;
  elements.loadMore.hidden = true;
  elements.grid.innerHTML = '<div class="skeleton-card"></div>'.repeat(6);
  elements.count.textContent = "正在准备模块库…";
  elements.syncTime.textContent = "";

  try {
    const response = await fetch(DATA_URL, { cache: "no-store" });
    if (!response.ok) throw new Error(`数据请求返回 ${response.status}`);
    const data = await response.json();
    if (!Array.isArray(data.plugins)) throw new Error("索引数据格式不正确");

    state.modules = publishable(data.plugins);
    state.visible = PAGE_SIZE;
    elements.statModules.textContent = state.modules.length;
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

/** 改动筛选条件后一律回到第一页，否则「加载更多」的分页会错位。 */
function refilter(change) {
  Object.assign(state, { visible: PAGE_SIZE }, change);
  renderModules();
}

elements.search.addEventListener("input", (event) => refilter({ query: event.target.value }));

elements.clearSearch.addEventListener("click", () => {
  elements.search.value = "";
  refilter({ query: "" });
  elements.search.focus();
});

elements.sort.addEventListener("change", (event) => refilter({ sort: event.target.value }));

elements.categories.addEventListener("click", (event) => {
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

elements.dialog.addEventListener("click", (event) => {
  if (event.target === elements.dialog) elements.dialog.close();
});

document.addEventListener("click", (event) => {
  const action = event.target.closest("[data-action]");
  if (action) {
    const module = findModule(action.dataset.id);
    if (module && action.dataset.action === "details") openDetails(module);
    if (module && action.dataset.action === "copy") copyModuleLink(module);
  }

  const dialogCopy = event.target.closest("[data-dialog-copy]");
  if (dialogCopy) {
    const module = findModule(dialogCopy.dataset.dialogCopy);
    if (module) copyModuleLink(module);
  }

  const install = event.target.closest("[data-install]");
  if (install) showToast(`正在唤起 Surge 导入「${install.dataset.install}」…`);
});

// 图标宿主可能拒连，破图直接摘掉，让底下的首字回退露出来。
document.addEventListener(
  "error",
  (event) => {
    if (event.target instanceof HTMLImageElement && event.target.classList.contains("module-icon")) {
      event.target.remove();
    }
  },
  true,
);

document.addEventListener("keydown", (event) => {
  const typing = ["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName);
  if (event.key === "/" && !typing && !elements.dialog.open) {
    event.preventDefault();
    elements.search.focus();
  }
  if (event.key === "Escape" && document.activeElement === elements.search && state.query) {
    elements.search.value = "";
    refilter({ query: "" });
  }
});

loadModules();
