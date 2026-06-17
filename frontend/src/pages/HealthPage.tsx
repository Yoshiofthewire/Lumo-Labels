import { useEffect, useMemo, useState } from "react";
import { getJSON } from "../api/client";

type Health = {
  healthy: boolean;
  unhealthyForSeconds: number;
  lastCheckUtc: string;
  failureReason: string[];
  aiCreditsExhausted?: boolean;
  aiCreditsExhaustedAt?: string;
};

type RunStatus = {
  scanIntervalSeconds: number;
  checkpoint: string;
};

function formatDuration(totalSeconds: number): string {
  if (totalSeconds <= 0) return "0s";
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const parts: string[] = [];
  if (days) parts.push(`${days}d`);
  if (hours) parts.push(`${hours}h`);
  if (minutes) parts.push(`${minutes}m`);
  if (seconds || parts.length === 0) parts.push(`${seconds}s`);
  return parts.join(" ");
}

export function HealthPage() {
  const [health, setHealth] = useState<Health | null>(null);
  const [runStatus, setRunStatus] = useState<RunStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [refreshEvery, setRefreshEvery] = useState(10);

  async function refreshHealth() {
    setLoading(true);
    try {
      const [nextHealth, nextStatus] = await Promise.all([
        getJSON<Health>("/api/health"),
        getJSON<RunStatus>("/api/status"),
      ]);
      setHealth(nextHealth);
      setRunStatus(nextStatus);
    } catch {
      setHealth(null);
      setRunStatus(null);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    refreshHealth();
  }, []);

  useEffect(() => {
    if (refreshEvery <= 0) return;
    const id = setInterval(refreshHealth, refreshEvery * 1000);
    return () => clearInterval(id);
  }, [refreshEvery]);

  const severityClass = health?.healthy ? "health-ok" : "health-bad";
  const lastChecked = useMemo(() => {
    if (!health?.lastCheckUtc) return "-";
    const d = new Date(health.lastCheckUtc);
    return Number.isNaN(d.getTime()) ? health.lastCheckUtc : d.toLocaleString();
  }, [health?.lastCheckUtc]);

  const creditsExhaustedAt = useMemo(() => {
    if (!health?.aiCreditsExhaustedAt) return "";
    const d = new Date(health.aiCreditsExhaustedAt);
    return Number.isNaN(d.getTime()) ? health.aiCreditsExhaustedAt : d.toLocaleString();
  }, [health?.aiCreditsExhaustedAt]);

  return (
    <section className="panel">
      <div className="health-head">
        <h2>Health Dashboard</h2>
        <div className="health-controls">
          <label>
            <span>Auto-refresh</span>
            <select value={refreshEvery} onChange={(e) => setRefreshEvery(Number(e.target.value))}>
              <option value={0}>Off</option>
              <option value={5}>5s</option>
              <option value={10}>10s</option>
              <option value={30}>30s</option>
            </select>
          </label>
          <button type="button" onClick={refreshHealth} disabled={loading}>
            {loading ? "Refreshing..." : "Refresh"}
          </button>
        </div>
      </div>

      {!health ? (
        <p>Waiting for health data.</p>
      ) : (
        <>
          <div className={`health-banner ${severityClass}`}>
            <strong>{health.healthy ? "System Healthy" : "System Unhealthy"}</strong>
            <span>Last checked: {lastChecked}</span>
          </div>

          {health.aiCreditsExhausted && (
            <div className="health-banner health-bad" style={{ marginTop: 10 }}>
              <strong>Llama AI credits exhausted</strong>
              <span>
                Email classification is paused until Llama credits reset
                {creditsExhaustedAt ? ` (since ${creditsExhaustedAt})` : ""}. It resumes automatically
                on the next successful classification.
              </span>
            </div>
          )}

          <div className="health-grid">
            <article className="health-card">
              <h4>Current Status</h4>
              <p className="health-value">{health.healthy ? "Healthy" : "Unhealthy"}</p>
            </article>
            <article className="health-card">
              <h4>Unhealthy Duration</h4>
              <p className="health-value">{formatDuration(health.unhealthyForSeconds ?? 0)}</p>
            </article>
            <article className="health-card">
              <h4>Failure Count</h4>
              <p className="health-value">{health.failureReason?.length ?? 0}</p>
            </article>
            <article className="health-card">
              <h4>Scan Interval</h4>
              <p className="health-value">{runStatus?.scanIntervalSeconds != null ? `${runStatus.scanIntervalSeconds}s` : "-"}</p>
            </article>
            <article className="health-card">
              <h4>Checkpoint</h4>
              <p className="health-value" style={{ fontSize: "0.95rem", fontWeight: 600 }}>
                {runStatus?.checkpoint ?? "-"}
              </p>
            </article>
          </div>

          <div className="health-card" style={{ marginTop: 14 }}>
            <h4>Failure Reasons</h4>
            {health.failureReason && health.failureReason.length > 0 ? (
              <ul className="health-list">
                {health.failureReason.map((reason, idx) => (
                  <li key={`${idx}-${reason}`}>{reason}</li>
                ))}
              </ul>
            ) : (
              <p>No active failure reasons.</p>
            )}
          </div>
        </>
      )}
    </section>
  );
}
