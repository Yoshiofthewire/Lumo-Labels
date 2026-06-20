import { useEffect, useRef, useState, useCallback } from "react";
import { getJSON } from "../api/client";

const LINE_OPTIONS = [50, 100, 200, 500, 1000];
const REFRESH_OPTIONS = [
  { label: "Off", value: 0 },
  { label: "5s", value: 5 },
  { label: "10s", value: 10 },
  { label: "30s", value: 30 },
];
const HIDDEN_LOG_PREFIXES = ["bootstrap", "proton"];
const HIDDEN_LOG_FILES = ["llama.log"];

// Files that should always appear first, in this order.
const PINNED_LOG_ORDER = ["app.log", "llama.log", "llama-error.log"];

type ProtonTokenDebugState = {
  path: string;
  exists: boolean;
  readable: boolean;
  parseable: boolean;
  size: number;
  modifiedAt?: string;
  updatedAt?: string;
  clientId?: string;
  uidPresent: boolean;
  accessTokenPresent: boolean;
  refreshTokenPresent: boolean;
  tokenReady: boolean;
  cookieCount: number;
  cookieNames?: string[];
  error?: string;
};

type ProtonTokenDebugResponse = {
  recommendedSource: "main" | "snapshot" | "none";
  main: ProtonTokenDebugState;
  snapshot: ProtonTokenDebugState;
  refresh?: {
    disabled: boolean;
    reason?: string;
  };
};

function sortLogFiles(files: string[]): string[] {
  const pinned = PINNED_LOG_ORDER.filter((f) => files.includes(f));
  const rest = files.filter((f) => !PINNED_LOG_ORDER.includes(f)).sort();
  return [...pinned, ...rest];
}

function tabLabel(filename: string): string {
  if (filename === "llama.log") return "Llama Server";
  return filename.replace(/\.log$/, "").replace(/[._-]/g, " ");
}

function levelClass(line: string): string {
  const l = line.toLowerCase();
  if (l.includes(" error") || l.includes("[error]") || l.includes("level=error")) return "log-error";
  if (l.includes(" warn")  || l.includes("[warn]")  || l.includes("level=warn"))  return "log-warn";
  if (l.includes(" info")  || l.includes("[info]")  || l.includes("level=info"))  return "log-info";
  if (l.includes(" debug") || l.includes("[debug]") || l.includes("level=debug")) return "log-debug";
  return "";
}

function yesNo(v: boolean): string {
  return v ? "Yes" : "No";
}

function formatIso(iso?: string): string {
  if (!iso) return "-";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "-";
  return d.toLocaleString();
}

function TokenFileCard({ title, state, recommended }: { title: string; state: ProtonTokenDebugState; recommended: boolean }) {
  return (
    <div style={{ border: "1px solid var(--line)", borderRadius: 8, padding: "0.7rem", background: "var(--bg)" }}>
      <div style={{ display: "flex", justifyContent: "space-between", gap: "0.5rem", alignItems: "center", marginBottom: "0.45rem" }}>
        <strong style={{ fontSize: "0.84rem" }}>{title}</strong>
        <span style={{ fontSize: "0.72rem", color: recommended ? "#90d7a9" : "var(--ink)", opacity: recommended ? 1 : 0.7 }}>
          {recommended ? "Recommended" : "Fallback"}
        </span>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(2, minmax(120px, 1fr))", gap: "0.35rem 0.6rem", fontSize: "0.75rem", fontFamily: "var(--mono)" }}>
        <span>Exists: {yesNo(state.exists)}</span>
        <span>Token ready: {yesNo(state.tokenReady)}</span>
        <span>Readable: {yesNo(state.readable)}</span>
        <span>Parseable: {yesNo(state.parseable)}</span>
        <span>UID: {yesNo(state.uidPresent)}</span>
        <span>Access: {yesNo(state.accessTokenPresent)}</span>
        <span>Refresh: {yesNo(state.refreshTokenPresent)}</span>
        <span>Cookies: {state.cookieCount}</span>
      </div>

      <div style={{ marginTop: "0.45rem", fontSize: "0.73rem", opacity: 0.85, fontFamily: "var(--mono)", display: "grid", gap: "0.22rem" }}>
        <div>Path: {state.path || "-"}</div>
        <div>Size: {state.size || 0}</div>
        <div>Updated: {formatIso(state.updatedAt)}</div>
        <div>Modified: {formatIso(state.modifiedAt)}</div>
        <div>Client ID: {state.clientId || "-"}</div>
        {state.cookieNames && state.cookieNames.length > 0 && <div>Cookie names: {state.cookieNames.join(", ")}</div>}
        {state.error && <div style={{ color: "#ff6b6b" }}>Error: {state.error}</div>}
      </div>
    </div>
  );
}

function ProtonTokenDebugCard() {
  const [data, setData] = useState<ProtonTokenDebugResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lastFetched, setLastFetched] = useState<Date | null>(null);
  const [refreshInterval, setRefreshInterval] = useState(10);

  const fetchDebug = useCallback(() => {
    setLoading(true);
    getJSON<ProtonTokenDebugResponse>("/api/debug/proton-token-state")
      .then((resp) => {
        setData(resp);
        setError(null);
        setLastFetched(new Date());
      })
      .catch((e) => {
        setError(e.message);
        setData(null);
      })
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    fetchDebug();
  }, [fetchDebug]);

  useEffect(() => {
    if (refreshInterval === 0) return;
    const id = setInterval(fetchDebug, refreshInterval * 1000);
    return () => clearInterval(id);
  }, [fetchDebug, refreshInterval]);

  return (
    <div style={{ marginBottom: "1rem", border: "1px solid var(--line)", borderRadius: 10, padding: "0.8rem", background: "rgba(56, 39, 30, 0.72)" }}>
      <div style={{ display: "flex", justifyContent: "space-between", gap: "0.8rem", alignItems: "center", flexWrap: "wrap" }}>
        <h3 style={{ margin: 0, fontSize: "0.95rem" }}>Proton Auth Debug</h3>
        <div style={{ display: "flex", gap: "0.55rem", alignItems: "center" }}>
          <label style={{ fontSize: "0.76rem", opacity: 0.75 }}>Refresh:</label>
          <select
            value={refreshInterval}
            onChange={(e) => setRefreshInterval(Number(e.target.value))}
            style={{ background: "var(--bg)", border: "1px solid var(--line)", borderRadius: 4, color: "var(--ink)", padding: "0.22rem 0.45rem", fontSize: "0.76rem" }}
          >
            {REFRESH_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
          {lastFetched && <span style={{ fontSize: "0.72rem", opacity: 0.7 }}>Updated {lastFetched.toLocaleTimeString()}</span>}
          <button onClick={fetchDebug} disabled={loading} style={{ fontSize: "0.78rem", padding: "0.28rem 0.62rem" }}>
            {loading ? "..." : "Refresh"}
          </button>
        </div>
      </div>

      {error && (
        <div style={{ color: "#ff6b6b", background: "rgba(255,107,107,0.1)", border: "1px solid rgba(255,107,107,0.3)", borderRadius: 4, padding: "0.45rem 0.65rem", marginTop: "0.65rem", fontSize: "0.8rem" }}>
          {error}
        </div>
      )}

      {data && (
        <>
          <div style={{ marginTop: "0.6rem", marginBottom: "0.6rem", fontSize: "0.78rem", opacity: 0.85 }}>
            Recommended source: <strong>{data.recommendedSource}</strong>
          </div>
          {data.refresh?.disabled && (
            <div style={{ marginBottom: "0.6rem", padding: "0.5rem 0.65rem", borderRadius: 6, border: "1px solid rgba(255,204,102,0.35)", background: "rgba(255,204,102,0.08)", fontSize: "0.78rem" }}>
              Proactive refresh is disabled{data.refresh.reason ? `: ${data.refresh.reason}` : ""}.
            </div>
          )}
          <div style={{ display: "grid", gap: "0.6rem", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))" }}>
            <TokenFileCard title="Main auth file" state={data.main} recommended={data.recommendedSource === "main"} />
            <TokenFileCard title="Snapshot auth file" state={data.snapshot} recommended={data.recommendedSource === "snapshot"} />
          </div>
        </>
      )}
    </div>
  );
}

function LogViewer({ filename }: { filename: string }) {
  const [lines, setLines]                     = useState<string[]>([]);
  const [lineCount, setLineCount]             = useState(200);
  const [refreshInterval, setRefreshInterval] = useState(10);
  const [filter, setFilter]                   = useState("");
  const [autoScroll, setAutoScroll]           = useState(true);
  const [loading, setLoading]                 = useState(false);
  const [lastFetched, setLastFetched]         = useState<Date | null>(null);
  const [error, setError]                     = useState<string | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);

  const fetchLogs = useCallback(() => {
    setLoading(true);
    getJSON<{ lines: string[] }>(`/api/logs?file=${encodeURIComponent(filename)}&lines=${lineCount}`)
      .then((data) => { setLines(data.lines ?? []); setLastFetched(new Date()); setError(null); })
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [filename, lineCount]);

  useEffect(() => { fetchLogs(); }, [fetchLogs]);

  useEffect(() => {
    if (refreshInterval === 0) return;
    const id = setInterval(fetchLogs, refreshInterval * 1000);
    return () => clearInterval(id);
  }, [fetchLogs, refreshInterval]);

  useEffect(() => {
    if (autoScroll && bottomRef.current) bottomRef.current.scrollIntoView({ behavior: "smooth" });
  }, [lines, autoScroll]);

  const filtered = filter.trim()
    ? lines.filter((l) => l.toLowerCase().includes(filter.toLowerCase()))
    : lines;

  return (
    <>
      <div style={{ display: "flex", gap: "0.5rem", alignItems: "center", flexWrap: "wrap", marginBottom: "0.6rem" }}>
        <input type="text" placeholder="Filter..." value={filter} onChange={(e) => setFilter(e.target.value)}
          style={{ background: "var(--bg)", border: "1px solid var(--line)", borderRadius: 4, color: "var(--ink)", padding: "0.3rem 0.6rem", fontFamily: "var(--mono)", fontSize: "0.8rem", width: 180 }} />
        <label style={{ fontSize: "0.8rem", opacity: 0.7 }}>Lines:</label>
        <select value={lineCount} onChange={(e) => setLineCount(Number(e.target.value))}
          style={{ background: "var(--bg)", border: "1px solid var(--line)", borderRadius: 4, color: "var(--ink)", padding: "0.3rem 0.5rem", fontSize: "0.8rem" }}>
          {LINE_OPTIONS.map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
        <label style={{ fontSize: "0.8rem", opacity: 0.7 }}>Refresh:</label>
        <select value={refreshInterval} onChange={(e) => setRefreshInterval(Number(e.target.value))}
          style={{ background: "var(--bg)", border: "1px solid var(--line)", borderRadius: 4, color: "var(--ink)", padding: "0.3rem 0.5rem", fontSize: "0.8rem" }}>
          {REFRESH_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
        <button onClick={fetchLogs} disabled={loading}
          style={{ background: "var(--accent-soft)", border: "1px solid var(--accent)", borderRadius: 4, color: "var(--ink)", padding: "0.3rem 0.8rem", cursor: loading ? "wait" : "pointer", fontSize: "0.8rem" }}>
          {loading ? "..." : "Refresh"}
        </button>
        <label style={{ marginLeft: "auto", display: "flex", gap: "0.4rem", alignItems: "center", cursor: "pointer", fontSize: "0.8rem" }}>
          <input type="checkbox" checked={autoScroll} onChange={(e) => setAutoScroll(e.target.checked)} style={{ accentColor: "var(--accent)" }} />
          Auto-scroll
        </label>
      </div>

      <div style={{ display: "flex", gap: "1rem", marginBottom: "0.4rem", fontSize: "0.72rem", opacity: 0.55 }}>
        <span>{filtered.length} line{filtered.length !== 1 ? "s" : ""}{filter ? " (filtered)" : ""}</span>
        {lastFetched && <span>Updated {lastFetched.toLocaleTimeString()}</span>}
        {refreshInterval > 0 && <span>Auto-refresh {refreshInterval}s</span>}
      </div>

      {error && (
        <div style={{ color: "#ff6b6b", background: "rgba(255,107,107,0.1)", border: "1px solid rgba(255,107,107,0.3)", borderRadius: 4, padding: "0.5rem 0.75rem", marginBottom: "0.5rem", fontSize: "0.85rem" }}>
          {error}
        </div>
      )}

      <pre style={{ background: "var(--bg)", border: "1px solid var(--line)", borderRadius: 6, padding: "0.75rem 1rem", overflowY: "auto", maxHeight: "60vh", margin: 0, fontSize: "0.78rem", fontFamily: "var(--mono)", lineHeight: 1.6 }}>
        {filtered.length === 0
          ? <span style={{ opacity: 0.4 }}>{loading ? "Loading..." : filter ? "No lines match filter." : "No log output yet."}</span>
          : filtered.map((line, i) => (
              <div key={i} className={"log-line " + levelClass(line)} style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{line}</div>
            ))
        }
        <div ref={bottomRef} />
      </pre>
    </>
  );
}

export function LogsPage() {
  const [files, setFiles]   = useState<string[]>([]);
  const [active, setActive] = useState<string>("app.log");

  useEffect(() => {
    getJSON<{ files: string[] }>("/api/logs/list")
      .then((d) => {
        const list = sortLogFiles(
          (d.files ?? []).filter(
            (name) => !HIDDEN_LOG_PREFIXES.some((prefix) => name.startsWith(prefix))
              && !HIDDEN_LOG_FILES.includes(name)
          )
        );
        setFiles(list);
        if (list.length > 0 && !list.includes(active)) setActive(list[0]);
      })
      .catch(() => {});
  }, []);

  return (
    <section className="panel">
      <h2 style={{ marginTop: 0 }}>Logs</h2>

      <ProtonTokenDebugCard />

      <div style={{ display: "flex", gap: 0, flexWrap: "wrap", borderBottom: "1px solid var(--line)", marginBottom: "1rem" }}>
        {files.map((f) => (
          <button key={f} onClick={() => setActive(f)}
            style={{
              background: active === f ? "var(--panel)" : "transparent",
              border: "1px solid var(--line)",
              borderBottom: active === f ? "1px solid var(--panel)" : "1px solid var(--line)",
              borderRadius: "4px 4px 0 0",
              marginBottom: active === f ? -1 : 0,
              color: active === f ? "var(--accent)" : "var(--ink)",
              padding: "0.35rem 0.9rem",
              cursor: "pointer",
              fontSize: "0.8rem",
              fontFamily: "var(--mono)",
              textTransform: "capitalize",
              whiteSpace: "nowrap",
            }}>
            {tabLabel(f)}
          </button>
        ))}
        {files.length === 0 && <span style={{ padding: "0.35rem 0.5rem", fontSize: "0.8rem", opacity: 0.4 }}>Loading...</span>}
      </div>

      {active && <LogViewer key={active} filename={active} />}
    </section>
  );
}
