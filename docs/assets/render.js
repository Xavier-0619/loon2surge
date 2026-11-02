// 纯渲染：给定模块数据，产出 HTML 片段。这里不碰应用状态，也不发请求。
// 所有插入模板的文本都必须经过 escapeHTML —— 模块名/描述来自上游，不可信。

const RAW_BASE = "https://raw.githubusercontent.com/Xavier-0619/loon2surge/main/surge/";
const SOURCE_BASE = "https://github.com/Xavier-0619/loon2surge/blob/main/surge/";

export const PAGE_SIZE = 24;
const FALLBACK_AUTHOR = "可莉插件中心";
const FALLBACK_DESC = "暂无模块描述，点击查看模块详情与原始文件。";

const ESCAPES = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };

export function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, (character) => ESCAPES[character]);
}

export function normalize(value) {
  return String(value ?? "").trim().toLocaleLowerCase("zh-CN");
}

/** 逐段编码：文件名可能带空格与中文，整段 encodeURIComponent 会连 "/" 一起编码。 */
function fileUrl(base, name) {
  return base + name.split("/").map(encodeURIComponent).join("/");
}

export function moduleUrl(module) {
  return fileUrl(RAW_BASE, `${module.stem}.sgmodule`);
}

export function sourcePageUrl(module) {
  return fileUrl(SOURCE_BASE, `${module.stem}.sgmodule`);
}

export function installUrl(module) {
  return `surge:///install-module?url=${encodeURIComponent(moduleUrl(module))}`;
}

/**
 * 能拿到的图标只认 https（http 会被浏览器当混合内容拦掉），
 * 取不到就回退到首字，卡片不会出现空白方块。
 */
function iconUrl(module) {
  const icon = String(module.icon ?? "").trim();
  return /^https:\/\//i.test(icon) ? icon : "";
}

function avatarTone(module) {
  let hash = 0;
  for (const character of module.stem) hash = (hash * 31 + character.codePointAt(0)) >>> 0;
  return hash % 6;
}

function avatarText(name) {
  return Array.from(String(name || "模块").replace(/\s/g, "")).slice(0, 2).join("");
}

function avatarMarkup(module, name) {
  const icon = iconUrl(module);
  const image = icon
    ? `<img class="module-icon" src="${escapeHTML(icon)}" alt="" loading="lazy" decoding="async" referrerpolicy="no-referrer">`
    : "";
  return `<div class="module-avatar tone-${avatarTone(module)}" aria-hidden="true">${image}<span class="avatar-fallback">${escapeHTML(avatarText(name))}</span></div>`;
}

function statusMarkup(module) {
  const label = module.adapted?.length ? "已适配" : "可导入";
  return `<span class="card-status"><svg class="icon"><use href="#i-check"/></svg>${label}</span>`;
}

function authorText(module) {
  return (module.authors ?? []).join(" / ") || FALLBACK_AUTHOR;
}

function tagList(module, limit = Infinity) {
  return (module.tags ?? []).slice(0, limit);
}

/** 同名插件融合后，主源会记下被并入的插件；在卡片上点明"一份顶多份"。 */
function mergedBadge(module) {
  const count = (module.merged?.length ?? 0) + 1;
  if (count < 2) return "";
  return `<span class="card-tag card-merged" title="由 ${count} 份同名插件融合，规则已合并">融合 ${count} 份</span>`;
}

export function cardMarkup(module, index) {
  const name = module.name || module.stem;
  const authors = authorText(module);
  const description = module.desc || FALLBACK_DESC;
  const tags = tagList(module, 2);
  const extra = tagList(module).length - tags.length;
  const date = module.date ? module.date.slice(0, 10) : "未知日期";
  const delay = Math.min(index % PAGE_SIZE, 10) * 24;

  return `
    <article class="module-card" style="animation-delay:${delay}ms">
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
        ${tags.map((tag) => `<span class="card-tag">${escapeHTML(tag)}</span>`).join("")}
        ${extra ? `<span class="card-tag">+${extra}</span>` : ""}
        ${mergedBadge(module)}
      </div>
      <div class="card-footer">
        <span class="card-date">${escapeHTML(date)}</span>
        <button class="icon-button card-action" type="button" data-action="copy" data-id="${escapeHTML(module.stem)}" aria-label="复制 ${escapeHTML(name)} 的模块链接" title="复制模块链接"><svg class="icon"><use href="#i-copy"/></svg></button>
        <a class="install-button" href="${escapeHTML(installUrl(module))}" data-install="${escapeHTML(name)}" aria-label="将 ${escapeHTML(name)} 导入 Surge"><svg class="icon"><use href="#i-download"/></svg>导入 Surge</a>
      </div>
    </article>`;
}

export function categoriesMarkup(categories, total) {
  const chips = categories.map(([tag, count]) => `<button class="category-chip" type="button" data-category="${escapeHTML(tag)}" aria-pressed="false">${escapeHTML(tag)} <span>${count}</span></button>`);
  return [
    `<button class="category-chip active" type="button" data-category="all" aria-pressed="true">全部模块 <span>${total}</span></button>`,
    ...chips,
  ].join("");
}

/** filtering 为真表示用户正在筛选，此时也要报出全量数字作参照。 */
export function resultsMarkup(matched, total, filtering) {
  if (filtering) return `找到 <strong>${matched}</strong> 个匹配模块 <span>· 全部 ${total} 个</span>`;
  return `共 <strong>${matched}</strong> 个可导入模块`;
}

function diagnosticList(title, items) {
  if (!Array.isArray(items) || items.length === 0) return "";
  const entries = items.map((item) => `<li>${escapeHTML(item)}</li>`).join("");
  return `<section class="diagnostic-section"><h3><svg class="icon"><use href="#i-info"/></svg>${escapeHTML(title)}</h3><ul class="diagnostic-list">${entries}</ul></section>`;
}

export function detailsMarkup(module) {
  const name = module.name || module.stem;
  const authors = authorText(module);
  const tags = tagList(module).map((tag) => `<span class="card-tag">${escapeHTML(tag)}</span>`).join("");
  return `
    <div class="dialog-header">
      ${avatarMarkup(module, name)}
      <div><h2 id="dialog-title">${escapeHTML(name)}</h2><p>by ${escapeHTML(authors)}</p></div>
    </div>
    <p class="dialog-description">${escapeHTML(module.desc || "暂无模块描述。")}</p>
    <div class="dialog-tags">${tags}${mergedBadge(module)}${statusMarkup(module)}</div>
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
}

const syncFormatter = new Intl.DateTimeFormat("zh-CN", {
  timeZone: "Asia/Shanghai",
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

/** 索引里的时间是 `2026-11-03 02:00:00 UTC`，换成北京时间给人看。 */
export function formatSyncTime(value) {
  if (!value) return "";
  const date = new Date(String(value).replace(" UTC", "Z").replace(" ", "T"));
  if (Number.isNaN(date.getTime())) return `数据同步于 ${value}`;
  return `数据同步于 ${syncFormatter.format(date)}（北京时间）`;
}
