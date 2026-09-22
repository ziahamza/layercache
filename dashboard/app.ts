type Estimate = { milliseconds: number | null; known: number; total: number; confidence: string };
type Report = { eligible: number; hits: number; misses: number; runs: number; degraded: boolean; netEstimatedBuildTimeSaved: Estimate; integrations: {integration: string; hits: number; eligible: number}[] | null };
type Cache = {state: string; reportState: string; status?: {usageBytes: number; maxBytes: number | null; artifacts: number; evictionPolicy: string}; report?: Report};
type Snapshot = {projects: {id: string; local: Cache; team: Cache}[]; updatedAt: string};

function consumeSessionFragment(): string {
  const value = location.hash.slice(1);
  if (!/^[a-f0-9]{64}$/.test(value)) return "";
  history.replaceState(null, "", location.pathname);
  return value;
}
let token = consumeSessionFragment();
const projects = document.querySelector<HTMLDivElement>("#projects")!;
const notice = document.querySelector<HTMLParagraphElement>("#notice")!;
const period = document.querySelector<HTMLSelectElement>("#period")!;
const refresh = document.querySelector<HTMLButtonElement>("#refresh")!;
const onboarding = document.querySelector<HTMLDialogElement>("#onboarding")!;
document.querySelector<HTMLButtonElement>("#connect")!.onclick = () => onboarding.showModal();
document.querySelector<HTMLAnchorElement>(".brand")!.onclick = (event) => { event.preventDefault(); projects.scrollIntoView(); };

function el<K extends keyof HTMLElementTagNameMap>(tag: K, className = "", text = ""): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag); node.className = className; node.textContent = text; return node;
}
function bytes(value: number): string { return value >= 2 ** 30 ? `${(value / 2 ** 30).toFixed(1)} GiB` : value >= 2 ** 20 ? `${(value / 2 ** 20).toFixed(1)} MiB` : value >= 1024 ? `${(value / 1024).toFixed(1)} KiB` : `${value} B`; }
function duration(value: number | null): string { if (value === null) return "Unknown"; const sign = value < 0 ? "−" : ""; const seconds = Math.abs(value) / 1000; return sign + (seconds >= 3600 ? `${(seconds / 3600).toFixed(1)} h` : seconds >= 60 ? `${(seconds / 60).toFixed(1)} min` : `${seconds.toFixed(1)} s`); }
function metric(label: string, value: string, detail: string): HTMLDivElement {
  const node = el("div"); node.append(el("div", "label", label), el("div", "value", value), el("div", "detail", detail)); return node;
}
function cacheView(name: string, cache: Cache): HTMLElement {
  const node = el("section", "cache");
  const heading = el("div", "cache-heading");
  const stateNames: Record<string, string> = {connected: "● Connected", unavailable: "Unavailable", "not-connected": "Not connected", "authentication-required": "Sign-in required"};
  heading.append(el("h3", "", name), el("span", `state ${cache.state}`, stateNames[cache.state] ?? "Unavailable")); node.append(heading);
  if (!cache.status) {
    const messages: Record<string, string> = {"not-connected": "Connect this project's Team Cache to share artifacts with engineers and CI. Choose Connect a project for setup instructions.", "authentication-required": "Run layercache login for this project's configuration, then refresh. Your account needs project access.", unavailable: name === "Local Cache" ? "Start this project's daemon with layercache start, then refresh. Cache usage and reports are unavailable while it cannot be reached." : "Team Cache could not be reached. Check the project's endpoint and connection, then refresh."};
    node.append(el("div", "empty", messages[cache.state] ?? "Cache information is unavailable.")); return node;
  }
  const {status, report} = cache;
  const stats = el("div", "stats");
  const usage = metric("Artifact storage", bytes(status.usageBytes), status.maxBytes === null ? "Capacity unavailable" : `of ${bytes(status.maxBytes)} artifact capacity`);
  if (status.maxBytes !== null && status.maxBytes > 0) { const meter = el("progress", "meter"); meter.max = status.maxBytes; meter.value = status.usageBytes; meter.setAttribute("aria-label", `${name} artifact capacity used`); usage.append(meter); }
  stats.append(usage, metric("Cache hit rate", report && report.eligible > 0 ? `${Math.round(report.hits / report.eligible * 100)}%` : "—", report ? report.eligible > 0 ? `${report.hits.toLocaleString()} hits · ${report.misses.toLocaleString()} misses` : "No eligible outcomes in this period" : cache.reportState === "authentication-required" ? "Sign in again to read reports" : "Report unavailable"));
  node.append(stats);
  const metrics = el("div", "metrics");
  const estimate = report && report.netEstimatedBuildTimeSaved.total > 0 ? report.netEstimatedBuildTimeSaved : undefined;
  metrics.append(metric("Stored artifacts", status.artifacts.toLocaleString(), `${status.evictionPolicy || "Unknown"} eviction policy`), metric("Net estimated time saved", estimate ? duration(estimate.milliseconds) : "Unknown", estimate ? `${estimate.known}/${estimate.total} known · ${estimate.confidence} confidence` : "Timing evidence unavailable")); node.append(metrics);
  const integrations = el("div", "integrations");
  for (const item of report?.integrations ?? []) integrations.append(el("span", "integration", `${item.integration} · ${item.hits}/${item.eligible} hits`));
  if (report?.degraded) integrations.append(el("span", "integration", "Partial or degraded evidence"));
  if (report) integrations.append(el("span", "integration", `${report.runs} observed runs`));
  node.append(integrations); return node;
}
let busy = false;
let activeRequest: AbortController | undefined;
async function load(): Promise<void> {
  if (busy) return;
  if (!token) { notice.textContent = "Open the complete dashboard link printed by the CLI to connect this browser. Reloading this page requires that link again."; return; }
  busy = true; refresh.disabled = true; period.disabled = true;
  const requestToken = token;
  const controller = new AbortController();
  activeRequest = controller;
  const timeout = setTimeout(() => controller.abort(), 35000);
  notice.textContent = "Refreshing cache data…";
  try {
    const response = await fetch(`/api/snapshot?period=${encodeURIComponent(period.value)}`, {headers: {Authorization: `Bearer ${requestToken}`}, signal: controller.signal});
    if (!response.ok) throw new Error(response.status === 401 ? "Session expired. Open the current dashboard link from the CLI." : response.status === 504 ? "Dashboard request timed out. Refresh to try again." : "Dashboard unavailable. Check that the CLI process is running.");
    const data: Snapshot = await response.json();
    if (requestToken !== token) return;
    projects.replaceChildren();
    for (const project of data.projects) {
      const card = el("article", "project"); const heading = el("div", "project-head"); const title = el("div");
      title.append(el("h2", "", project.id.split("/").at(-1) ?? project.id), el("p", "", project.id));
      heading.append(el("span", "project-icon", "▱"), title, el("span", "pill", "Connected via CLI"));
      const grid = el("div", "cache-grid"); grid.append(cacheView("Local Cache", project.local), cacheView("Team Cache", project.team));
      card.append(heading, grid); projects.append(card);
    }
    document.querySelector("#count")!.textContent = String(data.projects.length);
    document.querySelector("#updated")!.textContent = `Updated ${new Date(data.updatedAt).toLocaleTimeString()}`;
    notice.textContent = "Local and Team reports are separate observations; their savings should not be added together.";
  } catch (error) {
    if (requestToken !== token) return;
    projects.replaceChildren(); document.querySelector("#count")!.textContent = "—"; document.querySelector("#updated")!.textContent = "Snapshot unavailable";
    notice.textContent = controller.signal.aborted ? "Dashboard request timed out. Check the CLI connection and refresh." : error instanceof Error ? error.message : "Could not load cache data.";
  } finally {
    clearTimeout(timeout); activeRequest = undefined; busy = false; refresh.disabled = false; period.disabled = false;
    if (requestToken !== token) void load();
  }
}
window.addEventListener("hashchange", () => {
  const next = consumeSessionFragment();
  if (!next) return;
  if (busy && next === token) return;
  token = next;
  if (busy) activeRequest?.abort(); else void load();
});
refresh.onclick = () => { void load(); }; period.onchange = () => { void load(); }; void load();
