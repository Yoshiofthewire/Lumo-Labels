import { useEffect, useState } from "react";
import { getJSON, putJSON } from "../api/client";

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

export function TuningPage() {
  const [tuningText, setTuningText] = useState("");
  const [tuningStatus, setTuningStatus] = useState("");

  async function saveTuning() {
    try {
      const result = await putJSON<TuningSaveResponse>("/api/tuning", { content: tuningText });
      setTuningStatus(
        result.restartOk === false
          ? `Tuning saved, but Llama restart needs attention: ${result.restartError ?? "unknown restart failure"}`
          : "TUNING.md saved and Llama restarted."
      );
    } catch {
      setTuningStatus("Failed to save tuning file.");
    }
  }

  useEffect(() => {
    getJSON<TuningResponse>("/api/tuning")
      .then((tuningData) => {
        setTuningText(tuningData.content ?? "");
      })
      .catch(() => setTuningStatus("Failed to load tuning settings."));
  }, []);

  return (
    <section className="panel">
      <h2>TUNING.md</h2>
      <p>Edit and save the markdown instructions used for message labeling.</p>

      <label>
        <div>TUNING.md</div>
        <textarea rows={18} value={tuningText} onChange={(e) => setTuningText(e.target.value)} style={{ width: "100%" }} />
      </label>

      <button type="button" onClick={saveTuning}>Save TUNING.md</button>
      {tuningStatus ? <p>{tuningStatus}</p> : null}
    </section>
  );
}
