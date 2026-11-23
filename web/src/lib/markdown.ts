import type { ClarificationQuestion } from './types';

function escapeHtml(raw: string): string {
  return raw
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function renderInline(raw: string): string {
  let s = escapeHtml(raw);
  // Inline code first via placeholder tokens so emphasis never mangles code
  const codeTokens: string[] = [];
  s = s.replace(/`([^`\n]+)`/g, (_m, inner: string) => {
    const idx = codeTokens.length;
    codeTokens.push(`<code class="inline-code">${inner}</code>`);
    return `\u0000CODE${idx}\u0000`;
  });

  // Safe http/https links
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, (_m, label: string, url: string) => {
    return `<a href="${url}" target="_blank" rel="noopener noreferrer">${label}</a>`;
  });

  // Bold & italic
  s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/\*([^*\n]+)\*/g, '<em>$1</em>');

  return s.replace(/\u0000CODE(\d+)\u0000/g, (_m, idx: string) => codeTokens[Number(idx)] ?? '');
}

export function renderMarkdown(markdown: string): string {
  if (!markdown) return '';
  const src = markdown.replace(/\r\n/g, '\n');

  // Split out fenced code blocks cleanly first
  const blocks: string[] = [];
  const withPlaceholders = src.replace(/```([a-zA-Z0-9_-]*)\n([\s\S]*?)```/g, (_m, lang: string, code: string) => {
    const idx = blocks.length;
    const safeLang = escapeHtml(lang || 'text');
    const safeCode = escapeHtml(code.replace(/\n$/, ''));
    blocks.push(
      `<div class="code-block"><div class="code-head"><span>${safeLang}</span></div><pre><code>${safeCode}</code></pre></div>`
    );
    return `\n\u0000BLOCK${idx}\u0000\n`;
  });

  const lines = withPlaceholders.split('\n');
  const out: string[] = [];
  let inList: 'ul' | 'ol' | null = null;
  let para: string[] = [];

  const flushPara = () => {
    if (para.length > 0) {
      out.push(`<p>${renderInline(para.join(' '))}</p>`);
      para = [];
    }
  };
  const closeList = () => {
    if (inList) {
      out.push(`</${inList}>`);
      inList = null;
    }
  };

  for (const line of lines) {
    const trimmed = line.trim();
    const blockMatch = trimmed.match(/^\u0000BLOCK(\d+)\u0000$/);
    if (blockMatch) {
      flushPara();
      closeList();
      out.push(blocks[Number(blockMatch[1])] ?? '');
      continue;
    }
    if (trimmed === '') {
      flushPara();
      closeList();
      continue;
    }
    const heading = trimmed.match(/^(#{1,4})\s+(.+)$/);
    if (heading) {
      flushPara();
      closeList();
      const lvl = heading[1].length;
      out.push(`<h${lvl}>${renderInline(heading[2])}</h${lvl}>`);
      continue;
    }
    if (trimmed.startsWith('> ')) {
      flushPara();
      closeList();
      out.push(`<blockquote>${renderInline(trimmed.slice(2))}</blockquote>`);
      continue;
    }
    const ulMatch = trimmed.match(/^[-*]\s+(.+)$/);
    if (ulMatch) {
      flushPara();
      if (inList !== 'ul') {
        closeList();
        out.push('<ul>');
        inList = 'ul';
      }
      out.push(`<li>${renderInline(ulMatch[1])}</li>`);
      continue;
    }
    const olMatch = trimmed.match(/^\d+\.\s+(.+)$/);
    if (olMatch) {
      flushPara();
      if (inList !== 'ol') {
        closeList();
        out.push('<ol>');
        inList = 'ol';
      }
      out.push(`<li>${renderInline(olMatch[1])}</li>`);
      continue;
    }
    closeList();
    para.push(trimmed);
  }
  flushPara();
  closeList();
  return out.join('');
}

// Extracts structured Plan Clarification cards from fenced JSON / tether-clarify blocks
export function extractClarificationQuestions(text: string): ClarificationQuestion[] {
  if (!text || (!text.includes('"options"') && !text.includes('"questions"'))) {
    return [];
  }
  const found: ClarificationQuestion[] = [];
  const fenceRe = /```(?:tether-clarify|clarification|json)?\n([\s\S]*?)```/g;
  let match: RegExpExecArray | null;
  while ((match = fenceRe.exec(text)) !== null) {
    const raw = match[1].trim();
    if (!raw.startsWith('{') && !raw.startsWith('[')) continue;
    try {
      const parsed = JSON.parse(raw) as unknown;
      const list = Array.isArray(parsed)
        ? parsed
        : parsed && typeof parsed === 'object' && Array.isArray((parsed as { questions?: unknown[] }).questions)
          ? (parsed as { questions: unknown[] }).questions
          : [];
      for (let i = 0; i < list.length; i++) {
        const q = list[i] as Partial<ClarificationQuestion>;
        if (q && typeof q.question === 'string' && Array.isArray(q.options) && q.options.length > 0) {
          found.push({
            id: q.id || `q-${i + 1}`,
            question: q.question,
            options: q.options.map((o, j) => ({
              id: o.id || `opt-${j + 1}`,
              label: o.label || String(o.id ?? `Option ${j + 1}`),
              description: o.description,
              recommended: Boolean(o.recommended),
            })),
            allow_freeform: q.allow_freeform ?? true,
            selected_id: q.selected_id,
            freeform_answer: q.freeform_answer,
          });
        }
      }
    } catch {
      // Ignore non-clarification JSON blocks
    }
  }
  return found;
}
