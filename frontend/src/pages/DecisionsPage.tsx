import { useEffect, useState } from "react";
import { getJSON } from "../api/client";

type Decision = {
  messageId: string;
  sender: string;
  subject: string;
  label: string;
  status: string;
  detail: string;
  atUtc: string;
};

function formatTimestamp(value: string): string {
  if (!value) return "-";
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString();
}

function decisionText(item: Decision): string {
  if (item.label && item.label.trim() !== "") return item.label;
  if (item.status && item.status.trim() !== "") return item.status;
  return item.detail || "-";
}

export function DecisionsPage() {
  const [rows, setRows] = useState<Decision[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  async function loadDecisions() {
    setLoading(true);
    setError("");
    try {
      const data = await getJSON<Decision[]>("/api/decisions?limit=10");
      setRows(data ?? []);
    } catch (e) {
      const message = e instanceof Error ? e.message : "failed to load decisions";
      setError(message);
      setRows([]);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    loadDecisions();
    const interval = setInterval(loadDecisions, 10_000);
    return () => clearInterval(interval);
  }, []);

  return (
    <section className="panel">
      <h2>Decision Log</h2>
      <p>Last 10 labeling decisions.</p>

      <button type="button" onClick={loadDecisions} disabled={loading}>
        {loading ? "Loading..." : "Refresh"}
      </button>

      {error ? <p className="notice notice-error">Failed to load decisions: {error}</p> : null}

      {rows.length === 0 ? (
        <p>No decisions recorded yet.</p>
      ) : (
        <div style={{ overflowX: "auto", marginTop: 10 }}>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Email Subject</th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Decision</th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Time</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((item) => (
                <tr key={`${item.atUtc}-${item.messageId}`}>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>{item.subject || "(no subject)"}</td>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>{decisionText(item)}</td>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>{formatTimestamp(item.atUtc)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
