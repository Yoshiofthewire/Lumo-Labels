import { useEffect, useState } from "react";
import { getJSON, putJSON } from "../api/client";

type LabelsResponse = {
  configured: string[];
  proton: string[];
};

type TuningResponse = {
  content: string;
  path?: string;
};

type TuningSaveResponse = {
  ok: boolean;
  path?: string;
  restartOk?: boolean;
  restartError?: string;
};

const DEFAULT_LABELS = ["Questionable", "Primary", "Updates", "Social", "Promotions"];

function normalizeOrderedLabels(labels: string[]): string[] {
  const clean = labels.filter(Boolean);
  const unique = Array.from(new Set(clean));
  const hasQuestionable = unique.some((label) => label.toLowerCase() === "questionable");
  const rest = unique.filter((label) => label.toLowerCase() !== "questionable");
  return hasQuestionable ? ["Questionable", ...rest] : rest;
}

function normalizeLabelName(raw: string): string {
  return raw.replace(/^[-*]\s*/, "").replace(/:$/, "").trim();
}

function parsePriorityLabels(content: string): string[] {
  const lines = content.split(/\r?\n/);
  const labels: string[] = [];
  const seen = new Set<string>();

  const add = (value: string) => {
    const label = normalizeLabelName(value);
    if (!label) return;
    const key = label.toLowerCase();
    if (seen.has(key)) return;
    seen.add(key);
    labels.push(label);
  };

  const priorityAnchor = lines.findIndex((line) => /priority order/i.test(line));
  if (priorityAnchor >= 0) {
    for (let i = priorityAnchor + 1; i < lines.length; i += 1) {
      const line = lines[i];
      if (/^\s*##\s+/.test(line) || /^\s*###\s+/.test(line)) break;
      const match = line.match(/^\s*[-*]\s+(.+)$/);
      if (match) add(match[1]);
    }
  }

  if (labels.length > 0) return labels;

  const allowedAnchor = lines.findIndex((line) => /^\s*##\s+Allowed Labels\s*$/i.test(line));
  if (allowedAnchor >= 0) {
    for (let i = allowedAnchor + 1; i < lines.length; i += 1) {
      const line = lines[i];
      if (/^\s*##\s+/.test(line)) break;
      const match = line.match(/^\s*[-*]\s+(.+)$/);
      if (match) add(match[1]);
    }
  }

  return labels;
}

function parseDefinitions(content: string): Record<string, string> {
  const lines = content.split(/\r?\n/);
  const defs: Record<string, string> = {};

  const headingStyle = /^\s*###\s+(.+)\s*$/;
  let currentHeading = "";
  let headingLines: string[] = [];
  const flushHeading = () => {
    if (!currentHeading) return;
    const text = headingLines.join("\n").trim();
    if (text) defs[currentHeading] = text;
    currentHeading = "";
    headingLines = [];
  };

  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i];
    const headingMatch = line.match(headingStyle);
    if (headingMatch) {
      flushHeading();
      currentHeading = normalizeLabelName(headingMatch[1]);
      continue;
    }
    if (currentHeading) {
      if (/^\s*##\s+/.test(line)) flushHeading();
      else headingLines.push(line);
    }
  }
  flushHeading();

  const sectionAnchor = lines.findIndex((line) => /^\s*##\s+Label Definitions\s*$/i.test(line));
  if (sectionAnchor >= 0) {
    let i = sectionAnchor + 1;
    while (i < lines.length) {
      const line = lines[i];
      if (/^\s*##\s+/.test(line)) break;
      const labelMatch = line.match(/^\s*[-*]\s+([^:]+):\s*$/);
      if (!labelMatch) {
        i += 1;
        continue;
      }
      const label = normalizeLabelName(labelMatch[1]);
      i += 1;
      const chunks: string[] = [];
      while (i < lines.length) {
        const next = lines[i];
        if (/^\s*##\s+/.test(next) || /^\s*[-*]\s+[^:]+:\s*$/.test(next)) break;
        const bullet = next.match(/^\s*[-*]\s+(.+)$/);
        if (bullet) chunks.push(`- ${bullet[1].trim()}`);
        else if (next.trim() !== "") chunks.push(next.trim());
        i += 1;
      }
      const text = chunks.join("\n").trim();
      if (label && text) defs[label] = text;
    }
  }

  return defs;
}

function buildTuningTemplate(labels: string[], defs: Record<string, string>): string {
  const ordered = labels.filter(Boolean);
  const lines: string[] = [];
  lines.push("# Lumo Labeling Instructions");
  lines.push("");
  lines.push("You are Lumo. Use this document as the source of truth for assigning inbox labels.");
  lines.push("");
  lines.push("## Allowed Labels");
  lines.push("");
  for (const label of ordered) lines.push(`- ${label}`);
  lines.push("");
  lines.push("");
  lines.push("## Classification Rules");
  lines.push("");
  lines.push("1. Assign exactly one label per message.");
  lines.push("2. Prefer sender intent and message purpose over isolated keywords.");
  lines.push("3. If a message could fit multiple labels, use this priority order:");
  for (const label of ordered) lines.push(`\t - ${label}`);
  lines.push("4. If confidence is low, choose the most conservative non-promotional label.");
  lines.push("5. Return only the label string, exactly matching one of the allowed labels.");
  lines.push("");
  lines.push("## Label Definitions");
  lines.push("");
  for (const label of ordered) {
    lines.push(`- ${label}:`);
    const definition = defs[label]?.trim();
    if (definition) {
      for (const row of definition.split(/\r?\n/)) {
        const clean = row.replace(/^[-*]\s*/, "").trim();
        if (clean) lines.push(`\t- ${clean}`);
      }
    } else {
      lines.push("\t- Add guidance for this label.");
    }
  }
  lines.push("");
  lines.push("## User Tuning Notes");
  lines.push("");
  lines.push("The user may edit this file at any time. Always apply the latest version when labeling new messages.");
  lines.push("");
  return lines.join("\n");
}

export function TuningPage() {
  const [tuningText, setTuningText] = useState("");
  const [tuningStatus, setTuningStatus] = useState("");
  const [allLabels, setAllLabels] = useState<string[]>([]);
  const [orderedLabels, setOrderedLabels] = useState<string[]>([]);
  const [labelDefinitions, setLabelDefinitions] = useState<Record<string, string>>({});

  function hydrateFromTuning(content: string, fallbackLabels: string[]) {
    const parsedLabels = parsePriorityLabels(content);
    const parsedDefs = parseDefinitions(content);
    const merged = Array.from(new Set([...parsedLabels, ...Object.keys(parsedDefs), ...fallbackLabels])).filter(Boolean);
    setOrderedLabels(normalizeOrderedLabels(merged));
    setLabelDefinitions(parsedDefs);
  }

  function addDefaultLabels() {
    setOrderedLabels((prev) => {
      const merged = Array.from(new Set([...prev, ...DEFAULT_LABELS]));
      return normalizeOrderedLabels(merged);
    });
    setAllLabels((prev) => Array.from(new Set([...prev, ...DEFAULT_LABELS])));
    setTuningStatus("Default labels added.");
  }

  async function syncLabels() {
    try {
      const labelsData = await getJSON<LabelsResponse>("/api/labels");
      const fresh = Array.from(new Set([...(labelsData.proton ?? []), ...(labelsData.configured ?? [])])).filter(Boolean);
      setAllLabels(fresh);
      setOrderedLabels((prev) => {
        const keep = prev.filter((label) => fresh.includes(label));
        const add = fresh.filter((label) => !keep.includes(label));
        return normalizeOrderedLabels([...keep, ...add]);
      });
      setTuningStatus("Labels synced from Proton.");
    } catch {
      setTuningStatus("Failed to sync labels from Proton.");
    }
  }

  function moveLabel(index: number, direction: -1 | 1) {
    if (orderedLabels[index]?.toLowerCase() === "questionable") return;
    const nextIndex = index + direction;
    if (nextIndex < 0 || nextIndex >= orderedLabels.length) return;
    const next = [...orderedLabels];
    const [item] = next.splice(index, 1);
    next.splice(nextIndex, 0, item);
    setOrderedLabels(normalizeOrderedLabels(next));
  }

  function buildTuningFromLabels() {
    setTuningText(buildTuningTemplate(orderedLabels, labelDefinitions));
    setTuningStatus("Generated tuning content from current labels.");
  }

  function resetTuningTemplate() {
    const labels = orderedLabels.length > 0 ? orderedLabels : allLabels;
    setTuningText(buildTuningTemplate(labels, labelDefinitions));
    setTuningStatus("Tuning template reset using current labels.");
  }

  async function saveTuning() {
    try {
      const result = await putJSON<TuningSaveResponse>("/api/tuning", { content: tuningText });
      setTuningStatus(
        result.restartOk === false
          ? `Tuning saved, but Lumo restart needs attention: ${result.restartError ?? "unknown restart failure"}`
          : "Tuning saved and Lumo restarted. Guardrail and tuning will be reloaded before the next email or test runs."
      );
    } catch {
      setTuningStatus("Failed to save tuning file.");
    }
  }

  useEffect(() => {
    Promise.all([getJSON<LabelsResponse>("/api/labels"), getJSON<TuningResponse>("/api/tuning")])
      .then(([labelsData, tuningData]) => {
        const all = Array.from(new Set([...(labelsData.proton ?? []), ...(labelsData.configured ?? [])])).filter(Boolean);
        setAllLabels(all);
        const content = tuningData.content ?? "";
        setTuningText(content);
        hydrateFromTuning(content, all);
      })
      .catch(() => setTuningStatus("Failed to load tuning settings."));
  }, []);

  return (
    <section className="panel">
      <h2>Tuning Instructions</h2>
      <p>Edit TUNING.md content sent after GARDRAIL.md and before email content.</p>

      <h3>Proton Labels</h3>
      <button type="button" onClick={syncLabels}>Sync Labels from Proton</button>
      <button type="button" onClick={addDefaultLabels} style={{ marginLeft: 8 }}>Add Default Labels</button>
      {allLabels.length === 0 ? <p>No labels discovered yet.</p> : null}

      {orderedLabels.map((label, idx) => (
        <div key={label} style={{ border: "1px solid var(--line)", borderRadius: 6, padding: 8, marginBottom: 8 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
            <strong>{label}</strong>
            {label.toLowerCase() !== "questionable" ? (
              <>
                <button type="button" onClick={() => moveLabel(idx, -1)} disabled={idx === 0}>Up</button>
                <button type="button" onClick={() => moveLabel(idx, 1)} disabled={idx === orderedLabels.length - 1}>Down</button>
              </>
            ) : null}
          </div>
          {label.toLowerCase() === "questionable" ? (
            <p style={{ marginTop: 0, marginBottom: 8 }}></p>
          ) : null}
          <textarea
            rows={3}
            value={labelDefinitions[label] ?? ""}
            onChange={(e) =>
              setLabelDefinitions((prev) => ({
                ...prev,
                [label]: e.target.value
              }))
            }
            placeholder={label.toLowerCase() === "questionable" ? "Provided by built-in guardrails" : "Definition/instructions for this label"}
            disabled={label.toLowerCase() === "questionable"}
            style={{ width: "100%" }}
          />
        </div>
      ))}

      <button type="button" onClick={buildTuningFromLabels}>Build TUNING from Labels</button>
      <button type="button" onClick={resetTuningTemplate} style={{ marginLeft: 8 }}>Reset TUNING Template</button>

      <label>
        <div>TUNING.md</div>
        <textarea rows={18} value={tuningText} onChange={(e) => setTuningText(e.target.value)} style={{ width: "100%" }} />
      </label>

      <button type="button" onClick={saveTuning}>Save TUNING.md</button>
      {tuningStatus ? <p>{tuningStatus}</p> : null}
    </section>
  );
}
