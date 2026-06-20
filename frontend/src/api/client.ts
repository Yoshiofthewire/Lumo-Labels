async function requestJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: "include",
    ...init
  });
  if (!response.ok) {
    let detail = "";
    try {
      const contentType = response.headers.get("content-type") || "";
      if (contentType.includes("application/json")) {
        const data = await response.json() as { error?: string; message?: string };
        detail = data.error || data.message || "";
      } else {
        detail = (await response.text()).trim();
      }
    } catch {
      detail = "";
    }
    throw new Error(detail ? `request failed: ${response.status} - ${detail}` : `request failed: ${response.status}`);
  }
  return response.json() as Promise<T>;
}

export async function getJSON<T>(path: string): Promise<T> {
  return requestJSON<T>(path);
}

export async function putJSON<T>(path: string, body: unknown): Promise<T> {
  return requestJSON<T>(path, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json"
    },
    body: JSON.stringify(body)
  });
}

export async function postJSON<T>(path: string, body: unknown): Promise<T> {
  return requestJSON<T>(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/json"
    },
    body: JSON.stringify(body)
  });
}

export async function postFormData<T>(path: string, body: FormData): Promise<T> {
  return requestJSON<T>(path, {
    method: "POST",
    body
  });
}

export async function deleteJSON<T>(path: string): Promise<T> {
  return requestJSON<T>(path, {
    method: "DELETE"
  });
}
