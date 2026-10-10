// Shared modal shell: backdrop, card, header (eyebrow + title + optional
// actions), close button, focus trap, focus save/restore, and Escape-stack
// registration. Shells stay mounted and toggle the `hidden` attribute — the
// stylesheet keys modal visibility off `.modal[hidden]`.
//
// Clicking the backdrop, the × button, or any [data-close] element inside
// invokes onCloseRequest (callers decide to refuse while busy). A mouse
// click dismisses only when the press and the release land on the same
// [data-close] element, so a drag from the card onto the backdrop can't
// close it (the browser fires that click at their common ancestor).
// Escape pops
// this modal only when it is topmost (onEscape defaults to onCloseRequest).

import { useEffect, useRef } from "preact/hooks";
import type { ComponentChildren, HTMLAttributes, TargetedKeyboardEvent, TargetedMouseEvent } from "preact";
import { pushEscape, removeEscape } from "../modalStack";

export interface ModalProps {
	open: boolean;
	/** root id, e.g. "enrollModal" */
	id: string;
	/** dialog card id, e.g. "enrollCard" */
	cardId: string;
	/** extra classes on .modal-card (e.g. "wide") */
	cardClass?: string;
	/** busy lock: dims and blocks interaction via .locked */
	locked?: boolean;
	eyebrow: string;
	titleId: string;
	title: string;
	/** extra header content rendered under the title (photos rename editor) */
	headerExtras?: ComponentChildren;
	/** actions rendered next to the close button (photos Rename button) */
	headerActions?: ComponentChildren;
	body: ComponentChildren;
	/** element focused when opened; defaults to the card's .modal-close */
	initialFocus?: () => HTMLElement | null;
	backdropClickDisabled?: boolean;
	onCloseRequest: () => void;
	/** custom Escape behavior (photos modal cancels its rename edit first) */
	onEscape?: () => void;
	/** extra props for the root element (drag & drop handlers) */
	rootProps?: HTMLAttributes<HTMLDivElement>;
}

export function Modal(props: ModalProps) {
	const {
		open, id, cardId, cardClass, locked, eyebrow, titleId, title,
		headerExtras, headerActions, body, initialFocus,
		backdropClickDisabled, onCloseRequest, onEscape, rootProps,
	} = props;

	const cardRef = useRef<HTMLDivElement | null>(null);
	const lastFocus = useRef<HTMLElement | null>(null);

	// Always-current callbacks for the escape stack (which registers once per
	// open transition and must not capture stale props).
	const closeRef = useRef(onCloseRequest);
	closeRef.current = onCloseRequest;
	const escRef = useRef(onEscape ?? onCloseRequest);
	escRef.current = onEscape ?? onCloseRequest;

	// Escape-stack membership follows the open state.
	useEffect(() => {
		if (!open) return;
		pressDismissRef.current = null; // drop a stale press from the last open
		const esc = () => escRef.current();
		pushEscape(esc);
		return () => removeEscape(esc);
	}, [open]);

	// Focus management: focus the initial target on open, restore the
	// trigger on close/unmount. The callback lives in a ref so inline
	// closures (new identity every render) don't re-run this effect —
	// re-running it would steal focus back to the initial target while the
	// user types in any other field.
	const initialFocusRef = useRef(initialFocus);
	initialFocusRef.current = initialFocus;
	useEffect(() => {
		if (!open) return;
		lastFocus.current = document.activeElement as HTMLElement | null;
		const target = initialFocusRef.current?.() ?? cardRef.current?.querySelector<HTMLElement>(".modal-close");
		target?.focus();
		return () => {
			lastFocus.current?.focus?.();
		};
	}, [open]);

	// Keep Tab focus inside the dialog while it is open.
	const trapTab = (e: TargetedKeyboardEvent<HTMLDivElement>) => {
		if (e.key !== "Tab") return;
		const card = cardRef.current;
		if (!card) return;
		const focusables = [...card.querySelectorAll<HTMLElement>(
			"button, [href], input, select, textarea, summary, [tabindex]:not([tabindex='-1'])"
		)].filter((el) => {
			const btn = el as HTMLButtonElement | HTMLInputElement;
			return !("disabled" in btn && btn.disabled) && el.offsetParent !== null;
		});
		if (!focusables.length) return;
		const first = focusables[0];
		const last = focusables[focusables.length - 1];
		if (e.shiftKey && document.activeElement === first) { last.focus(); e.preventDefault(); }
		else if (!e.shiftKey && document.activeElement === last) { first.focus(); e.preventDefault(); }
	};

	// Backdrop / × / [data-close] clicks close (callers may refuse).
	// A mouse click only dismisses when the press AND the release land on
	// the same [data-close] element (the backdrop or a button): when a drag
	// starts inside the card and ends over the backdrop, the browser
	// dispatches the click at their common ancestor — this root div — which
	// must not count as a backdrop click, or backdropClickDisabled would be
	// bypassed (and enabled backdrops would close on drags too). Keyboard-
	// activated clicks fire without any mousedown and carry detail 0, so
	// they keep the release-only check.
	const pressDismissRef = useRef<Element | null>(null);
	const onMouseDown = (e: TargetedMouseEvent<HTMLDivElement>) => {
		pressDismissRef.current = (e.target as Element).closest?.("[data-close]") ?? null;
	};
	const onClick = (e: TargetedMouseEvent<HTMLDivElement>) => {
		const release = (e.target as Element).closest?.("[data-close]") ?? null;
		const press = pressDismissRef.current;
		pressDismissRef.current = null;
		if (press != null ? press === release : release != null && e.detail === 0) closeRef.current();
	};

	return (
		<div id={id} class={"modal" + (locked ? " locked" : "")} hidden={!open} {...rootProps} onClick={onClick} onMouseDown={onMouseDown}>
			<div class="modal-backdrop" data-close={backdropClickDisabled ? undefined : true} />
			<div
				class={"modal-card" + (cardClass ? ` ${cardClass}` : "")}
				id={cardId}
				role="dialog"
				aria-modal="true"
				aria-labelledby={titleId}
				ref={cardRef}
				onKeyDown={trapTab}
			>
				<header class="modal-head">
					<div>
						<p class="modal-eyebrow">{eyebrow}</p>
						<h2 id={titleId}>{title}</h2>
						{headerExtras}
					</div>
					{headerActions !== undefined ? (
						<div class="modal-head-actions">
							{headerActions}
							<button class="modal-close" type="button" data-close aria-label="Close">×</button>
						</div>
					) : (
						<button class="modal-close" type="button" data-close aria-label="Close">×</button>
					)}
				</header>
				{body}
			</div>
		</div>
	);
}
