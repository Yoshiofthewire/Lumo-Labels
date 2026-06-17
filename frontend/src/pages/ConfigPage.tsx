import { ChangeEvent, useEffect, useState } from "react";
import { getJSON, postFormData, postJSON, putJSON } from "../api/client";

type AppConfig = {
  timezone: string;
  logLevel: string;
  scan: { intervalSeconds: number };
  rateLimits: { perMinute: number; perHour: number };
  labels: { allowlist: string[] };
  llama: { baseUrl: string; apiKey: string; classifyPath: string };
};

type LabelsResponse = {
  configured: string[];
  proton: string[];
};

type TuningResponse = {
  content: string;
  path?: string;
};

type LlamaAuthStatus = {
  exists: boolean;
  path: string;
  size?: number;
  modifiedAt?: string;
  localEnabled: boolean;
};

type ProtonAuthStatus = {
  exists: boolean;
  path: string;
  size?: number;
  modifiedAt?: string;
  parseOk: boolean;
};

type ProtonAuthUploadResponse = {
  ok: boolean;
  path: string;
  filename: string;
  conversionMethod?: string;
  nextAction?: string;
  error?: string;
};

type ProtonPrivateKeyStatus = {
  keyExists: boolean;
  keyPath: string;
  keySize: number;
  keyModifiedAt?: string;
  passwordExists: boolean;
  passwordPath: string;
  passwordModifiedAt?: string;
  decryptReady: boolean;
};

type ProtonPrivateKeyUploadResponse = {
  ok: boolean;
  keyPath: string;
  passwordPath: string;
  filename?: string;
  passwordUpdated: boolean;
  decryptReady: boolean;
};

type LlamaAuthNoticeTone = "idle" | "success" | "warning" | "error";

function normalizeLabelName(raw: string): string {
  return raw.replace(/^[-*]\s*/, "").replace(/:$/, "").trim();
}

function parsePriorityLabels(content: string): string[] {
  const lines = content.split(/\r?\n/);
  const labels: string[] = [];
  const seen = new Set<string>();

  const add = (value: string) => {
    const label = normalizeLabelName(value);
    if (!label) {
      return;
    }
    const key = label.toLowerCase();
    if (seen.has(key)) {
      return;
    }
    seen.add(key);
    labels.push(label);
  };

  const priorityAnchor = lines.findIndex((line) => /priority order/i.test(line));
  if (priorityAnchor >= 0) {
    for (let i = priorityAnchor + 1; i < lines.length; i += 1) {
      const line = lines[i];
      if (/^\s*##\s+/.test(line) || /^\s*###\s+/.test(line)) {
        break;
      }
      const match = line.match(/^\s*[-*]\s+(.+)$/);
      if (match) {
        add(match[1]);
      }
    }
  }

  if (labels.length > 0) {
    return labels;
  }

  const allowedAnchor = lines.findIndex((line) => /^\s*##\s+Allowed Labels\s*$/i.test(line));
  if (allowedAnchor >= 0) {
    for (let i = allowedAnchor + 1; i < lines.length; i += 1) {
      const line = lines[i];
      if (/^\s*##\s+/.test(line)) {
        break;
      }
      const match = line.match(/^\s*[-*]\s+(.+)$/);
      if (match) {
        add(match[1]);
      }
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
    if (!currentHeading) {
      return;
    }
    const text = headingLines.join("\n").trim();
    if (text) {
      defs[currentHeading] = text;
    }
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
      if (/^\s*##\s+/.test(line)) {
        flushHeading();
      } else {
        headingLines.push(line);
      }
    }
  }
  flushHeading();

  const sectionAnchor = lines.findIndex((line) => /^\s*##\s+Label Definitions\s*$/i.test(line));
  if (sectionAnchor >= 0) {
    let i = sectionAnchor + 1;
    while (i < lines.length) {
      const line = lines[i];
      if (/^\s*##\s+/.test(line)) {
        break;
      }
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
        if (/^\s*##\s+/.test(next) || /^\s*[-*]\s+[^:]+:\s*$/.test(next)) {
          break;
        }
        const bullet = next.match(/^\s*[-*]\s+(.+)$/);
        if (bullet) {
          chunks.push(`- ${bullet[1].trim()}`);
        } else if (next.trim() !== "") {
          chunks.push(next.trim());
        }
        i += 1;
      }
      const text = chunks.join("\n").trim();
      if (label && text) {
        defs[label] = text;
      }
    }
  }

  return defs;
}

function buildTuningTemplate(labels: string[], defs: Record<string, string>): string {
  const ordered = labels.filter(Boolean);
  const lines: string[] = [];
  lines.push("# Llama Labeling Instructions");
  lines.push("");
  lines.push("You are Llama. Use this document as the source of truth for assigning inbox labels.");
  lines.push("");
  lines.push("## Allowed Labels");
  lines.push("");
  for (const label of ordered) {
    lines.push(`- ${label}`);
  }
  lines.push("");
  lines.push("");
  lines.push("## Classification Rules");
  lines.push("");
  lines.push("1. Assign exactly one label per message.");
  lines.push("2. Prefer sender intent and message purpose over isolated keywords.");
  lines.push("3. If a message could fit multiple labels, use this priority order:");
  for (const label of ordered) {
    lines.push(`\t - ${label}`);
  }
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
        if (clean) {
          lines.push(`\t- ${clean}`);
        }
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

export function ConfigPage() {
  const testPrompt = "Email Address: test@example.com  Subject Line: Llama connectivity test Return only the label Updates";
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [status, setStatus] = useState("");
  const [testResult, setTestResult] = useState("");
  const [testBusy, setTestBusy] = useState(false);
  const [tuningText, setTuningText] = useState("");
  const [tuningStatus, setTuningStatus] = useState("");
  const [allLabels, setAllLabels] = useState<string[]>([]);
  const [orderedLabels, setOrderedLabels] = useState<string[]>([]);
  const [labelDefinitions, setLabelDefinitions] = useState<Record<string, string>>({});
  const [protonAuth, setProtonAuth] = useState<ProtonAuthStatus | null>(null);
  const [protonAuthFile, setProtonAuthFile] = useState<File | null>(null);
  const [protonAuthStatus, setProtonAuthStatus] = useState("");
  const [protonAuthTone, setProtonAuthTone] = useState<LlamaAuthNoticeTone>("idle");
  const [protonAuthBusy, setProtonAuthBusy] = useState(false);
  const [protonPrivateKey, setProtonPrivateKey] = useState<ProtonPrivateKeyStatus | null>(null);
  const [protonPrivateKeyFile, setProtonPrivateKeyFile] = useState<File | null>(null);
  const [protonPrivateKeyPassword, setProtonPrivateKeyPassword] = useState("");
  const [protonPrivateKeyStatus, setProtonPrivateKeyStatus] = useState("");
  const [protonPrivateKeyTone, setProtonPrivateKeyTone] = useState<LlamaAuthNoticeTone>("idle");
  const [protonPrivateKeyBusy, setProtonPrivateKeyBusy] = useState(false);

  function hydrateFromTuning(content: string, fallbackLabels: string[]) {
    const parsedLabels = parsePriorityLabels(content);
    const parsedDefs = parseDefinitions(content);
    const merged = Array.from(new Set([...parsedLabels, ...fallbackLabels])).filter(Boolean);
    setOrderedLabels(merged);
    setLabelDefinitions(parsedDefs);
  }

  async function syncLabels() {
    try {
      const labelsData = await getJSON<LabelsResponse>("/api/labels");
      const fresh = Array.from(new Set([...(labelsData.proton ?? []), ...(labelsData.configured ?? [])])).filter(Boolean);
      setAllLabels(fresh);
      setOrderedLabels((prev) => {
        const keep = prev.filter((label) => fresh.includes(label));
        const add = fresh.filter((label) => !keep.includes(label));
        return [...keep, ...add];
      });
      setTuningStatus("Labels synced from Proton.");
    } catch {
      setTuningStatus("Failed to sync labels from Proton.");
    }
  }

  async function loadProtonAuthStatus() {
    try {
      const status = await getJSON<ProtonAuthStatus>("/api/proton/auth");
      setProtonAuth(status);
    } catch {
      setProtonAuthTone("error");
      setProtonAuthStatus("Failed to load Proton auth status.");
    }
  }

  async function loadProtonPrivateKeyStatus() {
    try {
      const status = await getJSON<ProtonPrivateKeyStatus>("/api/proton/private-key");
      setProtonPrivateKey(status);
    } catch {
      setProtonPrivateKeyTone("error");
      setProtonPrivateKeyStatus("Failed to load Proton private key status.");
    }
  }

  function resetTuningTemplate() {
    const labels = orderedLabels.length > 0 ? orderedLabels : allLabels;
    setTuningText(buildTuningTemplate(labels, labelDefinitions));
    setTuningStatus("Tuning template reset using current labels.");
  }

  useEffect(() => {
    Promise.all([
      getJSON<AppConfig>("/api/config"),
      getJSON<LabelsResponse>("/api/labels"),
	      getJSON<TuningResponse>("/api/tuning"),
	      getJSON<ProtonAuthStatus>("/api/proton/auth"),
	      getJSON<ProtonPrivateKeyStatus>("/api/proton/private-key")
    ])
        .then(([data, labelsData, tuningData, protonAuthData, protonPrivateKeyData]) => {
        setCfg(data);
        const all = Array.from(new Set([...(labelsData.proton ?? []), ...(labelsData.configured ?? [])])).filter(Boolean);
        setAllLabels(all);
        const content = tuningData.content ?? "";
        setTuningText(content);
        hydrateFromTuning(content, all);
        setProtonAuth(protonAuthData);
        setProtonPrivateKey(protonPrivateKeyData);
      })
      .catch(() => setStatus("Failed to load config. Please login first."));
  }, []);

  if (!cfg) {
    return (
      <section className="panel">
        <h2>Configuration</h2>
        <p>{status || "Loading configuration..."}</p>
      </section>
    );
  }

  async function save() {
    const next: AppConfig = { ...cfg };
    try {
      await putJSON<{ ok: boolean }>("/api/config", next);
      setCfg(next);
      setStatus("Configuration saved.");
    } catch {
      setStatus("Failed to save configuration.");
    }
  }

  function moveLabel(index: number, direction: -1 | 1) {
    const nextIndex = index + direction;
    if (nextIndex < 0 || nextIndex >= orderedLabels.length) {
      return;
    }
    const next = [...orderedLabels];
    const [item] = next.splice(index, 1);
    next.splice(nextIndex, 0, item);
    setOrderedLabels(next);
  }

  function buildTuningFromLabels() {
    setTuningText(buildTuningTemplate(orderedLabels, labelDefinitions));
    setTuningStatus("Generated tuning content from current labels.");
  }

  async function saveTuning() {
    try {
      await putJSON<{ ok: boolean; path?: string }>("/api/tuning", { content: tuningText });
      setTuningStatus("Tuning saved.");
    } catch {
      setTuningStatus("Failed to save tuning file.");
    }
  }

  async function runLlamaTest(): Promise<boolean> {
    setTestBusy(true);
    setTestResult("");
    try {
      const result = await postJSON<{ ok: boolean; response?: string; error?: string; baseUrl?: string; path?: string }>(
        "/api/llama/test",
        { prompt: testPrompt }
      );
      if (!result.ok) {
        setTestResult(`Llama test failed: ${result.error ?? "unknown error"}`);
        return false;
      } else {
        setTestResult(
          `Llama test passed\nBase URL: ${result.baseUrl ?? ""}\nPath: ${result.path ?? ""}\nResponse: ${result.response ?? ""}`
        );
        return true;
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : "unknown error";
      if (msg.includes("401")) {
        setTestResult("Llama test request failed: unauthorized (401). Please log in again.");
      } else {
        setTestResult(`Llama test request failed: ${msg}. Make sure Llama is reachable.`);
      }
      return false;
    } finally {
      setTestBusy(false);
    }
  }

  function onProtonAuthFileChange(event: ChangeEvent<HTMLInputElement>) {
    setProtonAuthFile(event.target.files?.[0] ?? null);
  }

  function onProtonPrivateKeyFileChange(event: ChangeEvent<HTMLInputElement>) {
    setProtonPrivateKeyFile(event.target.files?.[0] ?? null);
  }

  async function uploadProtonAuth() {
    if (!protonAuthFile) {
      setProtonAuthTone("warning");
      setProtonAuthStatus("Select the auth file generated by scripts/generate_mail_auth.js first.");
      return;
    }

    const form = new FormData();
    form.append("authFile", protonAuthFile);

    setProtonAuthBusy(true);
    setProtonAuthTone("idle");
    setProtonAuthStatus("");
    try {
      const protonResult = await postFormData<ProtonAuthUploadResponse>("/api/proton/auth", form);
      if (!protonResult.ok) {
        setProtonAuthTone("error");
        setProtonAuthStatus(protonResult.error ?? "Failed to convert Proton auth file.");
      } else {
        setProtonAuthTone("success");
        setProtonAuthStatus(
          `Proton auth converted from ${protonResult.filename}. ${protonResult.nextAction ?? "Restart the app container to apply new Proton tokens."}`
        );
      }

      setProtonAuthFile(null);
      await loadProtonAuthStatus();
    } catch (e) {
      const msg = e instanceof Error ? e.message : "unknown error";
      setProtonAuthTone("error");
      setProtonAuthStatus(`Failed to upload or convert Proton auth file: ${msg}`);
    } finally {
      setProtonAuthBusy(false);
    }
  }

  async function uploadProtonPrivateKey() {
    if (!protonPrivateKeyFile && protonPrivateKeyPassword.trim() === "") {
      setProtonPrivateKeyTone("warning");
      setProtonPrivateKeyStatus("Select a Proton private key file or enter the password to update the secret store.");
      return;
    }

    const form = new FormData();
    if (protonPrivateKeyFile) {
      form.append("keyFile", protonPrivateKeyFile);
    }
    if (protonPrivateKeyPassword.trim() !== "") {
      form.append("password", protonPrivateKeyPassword);
    }

    setProtonPrivateKeyBusy(true);
    setProtonPrivateKeyTone("idle");
    setProtonPrivateKeyStatus("");
    try {
      const result = await postFormData<ProtonPrivateKeyUploadResponse>("/api/proton/private-key", form);
      setProtonPrivateKeyTone("success");
      setProtonPrivateKeyStatus(
        result.decryptReady
          ? "Proton private key material saved. New encrypted messages will be decrypted before labeling."
          : "Proton private key material saved, but decryption is not ready yet. Upload both the key and its password."
      );
      setProtonPrivateKeyFile(null);
      setProtonPrivateKeyPassword("");
      await loadProtonPrivateKeyStatus();
    } catch (e) {
      const msg = e instanceof Error ? e.message : "unknown error";
      setProtonPrivateKeyTone("error");
      setProtonPrivateKeyStatus(`Failed to save Proton private key material: ${msg}`);
    } finally {
      setProtonPrivateKeyBusy(false);
    }
  }

  return (
    <section className="panel">
      <h2>Configuration</h2>
      <p>Manage authentication, tuning, and connectivity checks.</p>

      <hr />
      <h3>Proton Authentication</h3>
      <label>
        <div>Upload Mail auth from scripts/generate_mail_auth.js</div>
        <input type="file" accept="application/json,.json" onChange={onProtonAuthFileChange} />
      </label>
      <button type="button" onClick={uploadProtonAuth} disabled={protonAuthBusy}>
        {protonAuthBusy ? "Uploading Mail auth..." : "Upload Mail Auth"}
      </button>
      {protonAuth ? (
        <div style={{ border: "1px solid var(--line)", borderRadius: 6, padding: 10, marginTop: 10, marginBottom: 10 }}>
          <p>Token File Present: {protonAuth.exists ? "Yes" : "No"}</p>
          <p>Token File Path: {protonAuth.path}</p>
          {protonAuth.exists ? <p>Size: {protonAuth.size ?? 0} bytes</p> : null}
          {protonAuth.modifiedAt ? <p>Updated: {protonAuth.modifiedAt}</p> : null}
          <p>Parseable: {protonAuth.parseOk ? "Yes" : "No"}</p>
        </div>
      ) : null}
      {protonAuthStatus ? <p className={`notice notice-${protonAuthTone}`}>{protonAuthStatus}</p> : null}

      <hr />
      <h3>Proton Private Key</h3>
      <p>Upload the exported Proton private key and the password used to unlock it. These secrets are stored in the container-only private directory, not in the general config API.</p>
      <label>
        <div>Private key file</div>
        <input type="file" accept=".asc,.pgp,.txt" onChange={onProtonPrivateKeyFileChange} />
      </label>
      <label>
        <div>Private key password</div>
        <input
          type="password"
          value={protonPrivateKeyPassword}
          onChange={(event) => setProtonPrivateKeyPassword(event.target.value)}
          placeholder="Password used when exporting or unlocking the Proton private key"
        />
      </label>
      <button type="button" onClick={uploadProtonPrivateKey} disabled={protonPrivateKeyBusy}>
        {protonPrivateKeyBusy ? "Saving Proton key..." : "Save Proton Private Key"}
      </button>
      {protonPrivateKey ? (
        <div style={{ border: "1px solid var(--line)", borderRadius: 6, padding: 10, marginTop: 10, marginBottom: 10 }}>
          <p>Private Key Present: {protonPrivateKey.keyExists ? "Yes" : "No"}</p>
          <p>Private Key Path: {protonPrivateKey.keyPath}</p>
          {protonPrivateKey.keyExists ? <p>Private Key Size: {protonPrivateKey.keySize} bytes</p> : null}
          {protonPrivateKey.keyModifiedAt ? <p>Private Key Updated: {protonPrivateKey.keyModifiedAt}</p> : null}
          <p>Password Present: {protonPrivateKey.passwordExists ? "Yes" : "No"}</p>
          <p>Password Path: {protonPrivateKey.passwordPath}</p>
          {protonPrivateKey.passwordModifiedAt ? <p>Password Updated: {protonPrivateKey.passwordModifiedAt}</p> : null}
          <p>Decryption Ready: {protonPrivateKey.decryptReady ? "Yes" : "No"}</p>
        </div>
      ) : null}
      {protonPrivateKeyStatus ? <p className={`notice notice-${protonPrivateKeyTone}`}>{protonPrivateKeyStatus}</p> : null}

      <hr />
      <h3>Test Llama Connection</h3>
      <button type="button" onClick={runLlamaTest} disabled={testBusy}>
        {testBusy ? "Testing..." : "Run Llama Test"}
      </button>
      {testResult ? <pre>{testResult}</pre> : null}

      {status ? <p>{status}</p> : null}
    </section>
  );
}
