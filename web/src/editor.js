// CodeMirror for the compose and env editors. Loaded only on the new and
// edit pages. Each <textarea data-editor> gets an editor on top; the
// textarea stays in the form and gets the editor's text on every change,
// so the form posts the same way with or without JavaScript.
import { EditorView, basicSetup } from "codemirror";
import { EditorState, StateField, StateEffect } from "@codemirror/state";
import { Decoration, ViewPlugin } from "@codemirror/view";
import { yaml } from "@codemirror/lang-yaml";
import { indentWithTab } from "@codemirror/commands";
import { keymap } from "@codemirror/view";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags as t } from "@lezer/highlight";

// Env values for keys containing these words are blurred until clicked.
const SECRET = /^(\s*(?:export\s+)?)([A-Za-z_][A-Za-z0-9_.-]*)(\s*=\s*)(.*)$/;
const MARKERS = ["PASS", "SECRET", "TOKEN", "KEY"];
const isSecretKey = (k) => MARKERS.some((m) => k.toUpperCase().includes(m));

const reveal = StateEffect.define();
const revealed = StateField.define({
  create: () => new Set(),
  update(set, tr) {
    for (const e of tr.effects) if (e.is(reveal)) set = new Set(set).add(e.value);
    return set;
  },
});

const masked = Decoration.mark({ class: "cm-secret" });

function maskDecorations(view) {
  const shown = view.state.field(revealed);
  const ranges = [];
  for (const { from, to } of view.visibleRanges) {
    let pos = from;
    while (pos <= to) {
      const line = view.state.doc.lineAt(pos);
      const m = SECRET.exec(line.text);
      if (m && m[4] && isSecretKey(m[2]) && !shown.has(m[2])) {
        const start = line.from + m[1].length + m[2].length + m[3].length;
        ranges.push(masked.range(start, line.to));
      }
      pos = line.to + 1;
    }
  }
  return Decoration.set(ranges);
}

const envMask = ViewPlugin.fromClass(
  class {
    constructor(view) { this.decorations = maskDecorations(view); }
    update(u) {
      if (u.docChanged || u.viewportChanged || u.transactions.some((t) => t.effects.length)) {
        this.decorations = maskDecorations(u.view);
      }
    }
  },
  {
    decorations: (v) => v.decorations,
    eventHandlers: {
      mousedown(e, view) {
        if (!e.target.closest(".cm-secret")) return;
        const pos = view.posAtCoords({ x: e.clientX, y: e.clientY });
        if (pos == null) return;
        const m = SECRET.exec(view.state.doc.lineAt(pos).text);
        if (m) view.dispatch({ effects: reveal.of(m[2]) });
      },
    },
  },
);

// Files are edited on the same dark console surface they are shown on,
// in both themes.
const theme = EditorView.theme({
  "&": { fontSize: "13px", border: "1px solid var(--console-line)", borderRadius: "4px", background: "var(--console)", color: "var(--console-ink)" },
  ".cm-content": { fontFamily: "var(--mono)", caretColor: "var(--accent)" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--accent)", borderLeftWidth: "2px" },
  ".cm-gutters": { background: "var(--console-2)", color: "var(--console-mute)", borderRight: "1px solid var(--console-line)" },
  ".cm-activeLine": { background: "var(--console-2)" },
  ".cm-activeLineGutter": { background: "var(--console-line)", color: "var(--console-ink)" },
  "&.cm-focused": { outline: "2px solid var(--accent)", outlineOffset: "1px" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": { background: "var(--console-sel) !important" },
  ".cm-matchingBracket": { background: "var(--console-line)", outline: "1px solid var(--console-mute)" },
  ".cm-scroller": { minHeight: "8em", maxHeight: "70vh" },
}, { dark: true });

// YAML colours for the console. Keys carry the structure, so they get the
// accent; values stay plain so the file still reads as text.
const highlight = HighlightStyle.define([
  { tag: t.definition(t.propertyName), color: "#f2c14e" },
  { tag: t.string, color: "#9fd7a9" },
  { tag: t.content, color: "var(--console-ink)" },
  { tag: t.lineComment, color: "#7e8b97", fontStyle: "italic" },
  { tag: [t.labelName, t.typeName], color: "#94c1ff" },
  { tag: [t.separator, t.punctuation, t.squareBracket, t.brace], color: "#8a98a5" },
  { tag: t.meta, color: "#d58cf0" },
  { tag: t.keyword, color: "#d58cf0" },
]);

// CodeMirror injects a <style> element; the server's CSP only allows it
// with this response's nonce.
const nonce = document.querySelector('meta[name="csp-nonce"]')?.content;

for (const ta of document.querySelectorAll("textarea[data-editor]")) {
  const extensions = [basicSetup, keymap.of([indentWithTab]), theme, EditorView.lineWrapping,
    EditorState.tabSize.of(2),
    EditorView.updateListener.of((u) => { if (u.docChanged) ta.value = u.state.doc.toString(); })];
  if (nonce) extensions.push(EditorView.cspNonce.of(nonce));
  if (ta.dataset.editor === "yaml") extensions.push(yaml(), syntaxHighlighting(highlight));
  if (ta.dataset.editor === "env") extensions.push(revealed, envMask);
  const view = new EditorView({ doc: ta.value, extensions });
  ta.hidden = true;
  ta.after(view.dom);
}
