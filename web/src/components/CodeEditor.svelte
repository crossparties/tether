<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import { Compartment, EditorState } from '@codemirror/state';
  import { EditorView, drawSelection, highlightActiveLine, highlightActiveLineGutter, keymap, lineNumbers } from '@codemirror/view';
  import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
  import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
  import { javascript } from '@codemirror/lang-javascript';
  import { python } from '@codemirror/lang-python';
  import { markdown } from '@codemirror/lang-markdown';
  import { json } from '@codemirror/lang-json';
  import { tags } from '@lezer/highlight';
  import { app } from '../lib/store.svelte';

  let {
    value = '',
    filename = '',
    readonly = false,
    minHeight = '220px',
    onchange,
  }: {
    value: string;
    filename?: string;
    readonly?: boolean;
    minHeight?: string;
    onchange?: (next: string) => void;
  } = $props();

  let host: HTMLDivElement | undefined = $state();
  let view: EditorView | null = null;
  let lastEmitted = '';

  const langComp = new Compartment();
  const themeComp = new Compartment();
  const readOnlyComp = new Compartment();

  function pickLanguage(name: string) {
    const low = name.toLowerCase();
    if (low.endsWith('.py')) return python();
    if (low.endsWith('.md') || low.endsWith('.markdown')) return markdown();
    if (low.endsWith('.json')) return json();
    if (
      low.endsWith('.ts') ||
      low.endsWith('.tsx') ||
      low.endsWith('.js') ||
      low.endsWith('.jsx') ||
      low.endsWith('.mjs') ||
      low.endsWith('.svelte')
    ) {
      return javascript({ typescript: low.endsWith('.ts') || low.endsWith('.tsx'), jsx: true });
    }
    return [];
  }

  function makeCatppuccinTheme(mode: 'mocha' | 'latte') {
    const dark = mode === 'mocha';
    const baseTheme = EditorView.theme(
      {
        '&': {
          backgroundColor: dark ? '#1E1E2E' : '#EFF1F5',
          color: dark ? '#CDD6F4' : '#4C4F69',
          fontFamily: 'var(--font-mono)',
          fontSize: '12.5px',
          height: '100%',
        },
        '.cm-scroller': {
          overflow: 'auto',
          fontFamily: 'var(--font-mono)',
          lineHeight: '1.52',
        },
        '.cm-content': {
          caretColor: dark ? '#CBA6F7' : '#8839EF',
          padding: '8px 0',
        },
        '.cm-gutters': {
          backgroundColor: dark ? '#181825' : '#E6E9EF',
          color: dark ? '#6C7086' : '#6C6F85',
          borderRight: `1px solid ${dark ? 'rgba(180,190,254,0.18)' : 'rgba(114,135,253,0.22)'}`,
        },
        '.cm-activeLine': {
          backgroundColor: dark ? 'rgba(49,50,68,0.45)' : 'rgba(204,208,218,0.45)',
        },
        '.cm-activeLineGutter': {
          backgroundColor: dark ? '#313244' : '#CCD0DA',
          color: dark ? '#B4BEFE' : '#7287FD',
        },
        '&.cm-focused .cm-cursor': {
          borderLeftColor: dark ? '#CBA6F7' : '#8839EF',
        },
        '&.cm-focused .cm-selectionBackground, ::selection': {
          backgroundColor: dark ? 'rgba(180,190,254,0.24)' : 'rgba(114,135,253,0.24)',
        },
      },
      { dark }
    );

    const hl = HighlightStyle.define([
      { tag: [tags.keyword, tags.operatorKeyword], color: dark ? '#CBA6F7' : '#8839EF' },
      { tag: [tags.function(tags.variableName), tags.labelName], color: dark ? '#89B4FA' : '#1E66F5' },
      { tag: [tags.string, tags.special(tags.string)], color: dark ? '#A6E3A1' : '#40A02B' },
      { tag: [tags.number, tags.bool, tags.atom], color: dark ? '#FAB387' : '#FE640B' },
      { tag: [tags.typeName, tags.className, tags.propertyName], color: dark ? '#F9E2AF' : '#DF8E1D' },
      { tag: [tags.comment, tags.meta], color: dark ? '#6C7086' : '#6C6F85', fontStyle: 'italic' },
      { tag: [tags.heading], color: dark ? '#CBA6F7' : '#8839EF', fontWeight: '700' },
      { tag: [tags.invalid], color: dark ? '#F38BA8' : '#D20F39' },
    ]);

    return [baseTheme, syntaxHighlighting(hl)];
  }

  onMount(() => {
    if (!host) return;
    lastEmitted = value;
    const state = EditorState.create({
      doc: value,
      extensions: [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightActiveLine(),
        drawSelection(),
        history(),
        keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
        langComp.of(pickLanguage(filename)),
        themeComp.of(makeCatppuccinTheme(app.theme)),
        readOnlyComp.of(EditorState.readOnly.of(readonly)),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && onchange) {
            const next = u.state.doc.toString();
            lastEmitted = next;
            onchange(next);
          }
        }),
      ],
    });
    view = new EditorView({ state, parent: host });
  });

  $effect(() => {
    if (!view) return;
    const nextTheme = app.theme;
    view.dispatch({ effects: themeComp.reconfigure(makeCatppuccinTheme(nextTheme)) });
  });

  $effect(() => {
    if (!view) return;
    view.dispatch({ effects: langComp.reconfigure(pickLanguage(filename)) });
  });

  $effect(() => {
    if (!view) return;
    view.dispatch({ effects: readOnlyComp.reconfigure(EditorState.readOnly.of(readonly)) });
  });

  $effect(() => {
    if (!view) return;
    const cur = view.state.doc.toString();
    if (value !== cur && value !== lastEmitted) {
      lastEmitted = value;
      view.dispatch({
        changes: { from: 0, to: cur.length, insert: value },
      });
    }
  });

  onDestroy(() => {
    view?.destroy();
    view = null;
  });
</script>

<div class="cm-host" style:min-height={minHeight} bind:this={host}></div>

<style>
  .cm-host {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    overflow: hidden;
    height: 100%;
    display: flex;
    flex-direction: column;
  }

  .cm-host :global(.cm-editor) {
    height: 100%;
    flex: 1;
    min-height: 0;
  }
</style>
