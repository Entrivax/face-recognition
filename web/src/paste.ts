// Clipboard routing: image pastes go to the component that currently wants
// them. Components opt in with usePasteFiles(enabled, handler, key) — a
// modal passes its injected isTopmost prop as the flag, the stage passes
// true under the reserved PASTE_FALLBACK key. The LAST registration wins,
// which is the topmost open modal (the modal pool mounts instances in
// stacking order and re-registers on isTopmost flips); with no interested
// modal open, the "*" fallback (the stage) receives the files. Plain-text
// pastes are never hijacked: pastes without image files and pastes into
// text fields carrying text are left to the browser.
//
// The document paste listener is installed while at least one registration
// exists and removed when the last one goes away.

import { createContext } from "preact";
import { useContext, useEffect, useRef } from "preact/hooks";
import type { ModalKey } from "./modalRegistry";
import { imageFilesFromClipboard } from "./util";

/** Reserved registration key: the route of last resort — normally the stage. */
export const PASTE_FALLBACK = "*";

export type PasteHandler = (files: File[]) => void;

/** The key of the modal instance currently being rendered (provided by the
 *  modal service around every pooled instance, so its components never have
 *  to know their own key). Null outside one. */
export const ModalKeyContext = createContext<string | null>(null);

type Reg = { key: string; handler: PasteHandler };

// Module-level registration stack + listener: one document listener serves
// every ModalProvider and every view (they never overlap in one tree).
const stack: Reg[] = [];

function onPasteEvent(e: ClipboardEvent) {
	const files = imageFilesFromClipboard(e.clipboardData);
	if (!files.length) return; // normal text paste — leave to the browser

	const t = e.target as HTMLElement | null;
	if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA") &&
			typeof e.clipboardData?.getData === "function" &&
			e.clipboardData.getData("text/plain")) {
		return; // text field + text on the clipboard: default behaviour wins
	}
	e.preventDefault();

	// Topmost interest wins (the last registration — see the module header).
	const reg = stack[stack.length - 1];
	if (reg) reg.handler(files);
}

/**
 * Register a paste handler while `enabled`. The handler lives in a ref, so
 * the call site may pass an inline closure (it is read fresh on every
 * paste). The registration key comes from the modal instance context; pass
 * it explicitly only when calling outside one (the stage uses
 * PASTE_FALLBACK).
 */
export function usePasteFiles(
	enabled: boolean,
	handler: PasteHandler,
	key?: ModalKey | typeof PASTE_FALLBACK,
): void {
	const instKey = useContext(ModalKeyContext);
	const regKey = key ?? instKey;
	if (!regKey) throw new Error("usePasteFiles: no modal key (inside an instance) or explicit key");

	// Always-current handler for the mount-stable envelope below.
	const handlerRef = useRef(handler);
	handlerRef.current = handler;

	useEffect(() => {
		if (!enabled) return;
		const reg: Reg = { key: regKey, handler: (files) => handlerRef.current(files) };
		const first = stack.length === 0;
		stack.push(reg);
		if (first) document.addEventListener("paste", onPasteEvent);
		return () => {
			const i = stack.lastIndexOf(reg);
			if (i >= 0) stack.splice(i, 1);
			if (stack.length === 0) document.removeEventListener("paste", onPasteEvent);
		};
	}, [enabled, regKey]);
}
