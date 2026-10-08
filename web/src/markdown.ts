// Markdown rendering for the person-details description: marked parses the
// CommonMark subset and DOMPurify strips everything prose does not need
// (scripts, event handlers, dangerous URIs), so admin-written markdown can be
// injected as HTML without opening an XSS hole on the public details modal.
// Links get rel="noopener noreferrer" + target="_blank" via a hook.
//
// Single newlines break lines (breaks: true) — the field is free-form notes,
// and hard-wrapping should not require trailing spaces.

import DOMPurify from "dompurify";
import { marked } from "marked";

marked.setOptions({ gfm: true, breaks: true, async: false });

DOMPurify.addHook("afterSanitizeAttributes", (node) => {
	if (node instanceof Element && node.tagName === "A") {
		node.setAttribute("target", "_blank");
		node.setAttribute("rel", "noopener noreferrer");
	}
});

/** Render a markdown description to sanitized HTML. */
export function renderMarkdown(src: string): string {
	const html = marked.parse(src ?? "", { async: false });
	return DOMPurify.sanitize(html as string);
}
