// Shared modal shell: backdrop, card, header (eyebrow + title + optional
// actions), close button, focus trap, focus save/restore, and Escape-stack
// registration. Shells stay mounted and toggle the `hidden` attribute — the
// stylesheet keys modal visibility off `.modal[hidden]`.
//
// Clicking the backdrop, the × button, or any [data-close] element inside
// invokes onCloseRequest (callers decide to refuse while busy). Escape pops
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
		const esc = () => escRef.current();
		pushEscape(esc);
		return () => removeEscape(esc);
	}, [open]);

	// Focus management: focus the initial target on open, restore the
	// trigger on close/unmount.
	useEffect(() => {
		if (!open) return;
		lastFocus.current = document.activeElement as HTMLElement | null;
		const target = initialFocus?.() ?? cardRef.current?.querySelector<HTMLElement>(".modal-close");
		target?.focus();
		return () => {
			lastFocus.current?.focus?.();
		};
	}, [open, initialFocus]);

	// Keep Tab focus inside the dialog while it is open.
	const trapTab = (e: TargetedKeyboardEvent<HTMLDivElement>) => {
		if (e.key !== "Tab") return;
		const card = cardRef.current;
		if (!card) return;
		const focusables = [...card.querySelectorAll<HTMLElement>(
			"button, [href], input, select, textarea, [tabindex]:not([tabindex='-1'])"
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
	const onClick = (e: TargetedMouseEvent<HTMLDivElement>) => {
		if (e.target === e.currentTarget || (e.target as Element).closest?.("[data-close]")) closeRef.current();
	};

	return (
		<div id={id} class={"modal" + (locked ? " locked" : "")} hidden={!open} {...rootProps} onClick={onClick}>
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
