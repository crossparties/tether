let csrfToken = '';

export function setCsrfToken(token: string) {
  csrfToken = token ?? '';
}

export function getCsrfToken(): string {
  return csrfToken;
}

export class ApiError extends Error {
  status: number;
  data: Record<string, unknown> | null;

  constructor(message: string, status: number, data: Record<string, unknown> | null = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.data = data;
  }
}

const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export async function apiFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method ?? 'GET').toUpperCase();
  const headers = new Headers(init.headers);
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (MUTATING.has(method) && csrfToken) {
    headers.set('X-Tether-Token', csrfToken);
  }

  const resp = await fetch(path, {
    ...init,
    method,
    headers,
    credentials: 'same-origin',
  });

  const text = await resp.text();
  let parsed: unknown = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { error: text };
    }
  }

  if (!resp.ok) {
    const obj = (parsed && typeof parsed === 'object' ? parsed : null) as Record<string, unknown> | null;
    const msg =
      (obj && typeof obj.error === 'string' && obj.error) ||
      `${method} ${path} failed (${resp.status})`;
    throw new ApiError(msg, resp.status, obj);
  }

  return parsed as T;
}

export async function redeemBootstrapToken(rawToken: string): Promise<{ authenticated: boolean; csrf_token: string }> {
  const trimmed = rawToken.trim();
  // Support pasting either raw hex token or full bootstrap URL containing ?token=...
  let tok = trimmed;
  if (trimmed.includes('token=')) {
    try {
      const u = new URL(trimmed, window.location.origin);
      tok = u.searchParams.get('token') ?? trimmed;
    } catch {
      const m = trimmed.match(/token=([a-fA-F0-9]+)/);
      if (m) tok = m[1];
    }
  }
  const resp = await fetch(`/auth?token=${encodeURIComponent(tok)}`, {
    method: 'GET',
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  });
  const data = (await resp.json().catch(() => ({}))) as { authenticated?: boolean; csrf_token?: string; error?: string };
  if (!resp.ok || !data.authenticated) {
    throw new ApiError(data.error ?? `Token redemption failed (${resp.status})`, resp.status, data);
  }
  if (data.csrf_token) {
    setCsrfToken(data.csrf_token);
  }
  return { authenticated: true, csrf_token: data.csrf_token ?? '' };
}
