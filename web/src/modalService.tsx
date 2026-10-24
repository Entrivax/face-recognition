// Modal service: a pool of named modal instances provided by context, so any
// component can open, close, or update a dialog itself instead of a shell
// component owning one open-state per modal and wiring every interaction.
//
// A "definition" maps a modal key to its component. The provider keeps an
// ORDERED pool of instances ({ key, args }); the pool renders in order, so a
// later instance paints on top of an earlier one — the .modal rule is
// position:fixed and stacking comes from DOM order, the same mechanism the
// previous static JSX list used.
//
// Lifecycle:
//  - open(key, args?) creates the instance (it mounts = is open) or replaces
//    the args of the existing one in place — one instance per key, no
//    duplicates, no reordering, so the pool stays append-only and mirrors
//    the escape stack that Modal maintains in modalStack.
//  - close(...keys) REMOVES the instances → they unmount. Focus save/restore
//    and escape-stack removal happen through Modal's own open-effect
//    cleanups, which run on unmount exactly as they did on open→false.
//  - patch(key, partial, when?) shallow-merges the args of a mounted
//    instance — how one modal live-updates another (face results arriving
//    for the photo the check viewer shows, a rename propagating from the
//    photos manager to the details modal underneath). The optional `when`
//    guard targets the update; patch is a no-op when the instance is absent
//    or the guard refuses.
//
// "Paused" modals: an instance with another one above it stays mounted with
// its state intact — only an explicit close() removes it.
//
// Each instance receives the positional lifecycle props: open (always true
// while mounted), onCloseRequest, and isTopmost (true while no other
// instance sits above it — read by the modals that need it, e.g. the
// clipboard routing in paste.ts; the service itself carries no paste code).

import { createContext } from "preact";
import { useCallback, useContext, useEffect, useMemo, useState } from "preact/hooks";
import type { ComponentChildren, ComponentType } from "preact";
import { handleEscape } from "./modalStack";
import { ModalKeyContext } from "./paste";

export interface ModalInstance {
	key: string;
	args: Record<string, unknown>;
}

export interface ModalService {
	open: (key: string, args?: Record<string, unknown>) => void;
	close: (...keys: string[]) => void;
	patch: (
		key: string,
		partial: Record<string, unknown>,
		when?: (current: Record<string, unknown> | null) => boolean,
	) => void;
}

const ModalContext = createContext<ModalService | null>(null);

/** The modal pool service; throws when rendered outside a ModalProvider. */
export function useModalService(): ModalService {
	const v = useContext(ModalContext);
	if (!v) throw new Error("useModalService: no ModalProvider above this component");
	return v;
}

interface ModalProviderProps {
	/** modal key → component (rendered with its args + open/onCloseRequest) */
	defs: Record<string, ComponentType<any>>;
	children: ComponentChildren;
}

export function ModalProvider({ defs, children }: ModalProviderProps) {
	const [pool, setPool] = useState<ModalInstance[]>([]);

	const open = useCallback((key: string, args: Record<string, unknown> = {}) => {
		setPool((prev) => {
			const i = prev.findIndex((m) => m.key === key);
			if (i < 0) return [...prev, { key, args }];
			const next = prev.slice();
			next[i] = { key, args };
			return next;
		});
	}, []);

	const close = useCallback((...keys: string[]) => {
		if (!keys.length) return;
		const gone = new Set(keys);
		setPool((prev) => prev.filter((m) => !gone.has(m.key)));
	}, []);

	const patch = useCallback((
		key: string,
		partial: Record<string, unknown>,
		when?: (current: Record<string, unknown> | null) => boolean,
	) => {
		setPool((prev) => {
			const i = prev.findIndex((m) => m.key === key);
			if (i < 0) return prev;
			const inst = prev[i];
			if (when && !when(inst.args)) return prev;
			const next = prev.slice();
			next[i] = { key, args: { ...inst.args, ...partial } };
			return next;
		});
	}, []);

	// Everything dies with the provider — and the scroll lock too (a logout
	// unmounts the whole tree; the class must not survive it).
	useEffect(() => () => {
		document.body.classList.remove("modal-open");
	}, []);

	// Body scroll lock while any modal is open (mounted == open here).
	useEffect(() => {
		document.body.classList.toggle("modal-open", pool.length > 0);
	}, [pool.length]);

	// Escape closes the topmost open dialog (modalStack registry, filled by
	// each Modal's open-effect; custom Escape behavior lives in the modal).
	useEffect(() => {
		const onKey = (e: KeyboardEvent) => {
			if (e.key === "Escape") handleEscape();
		};
		document.addEventListener("keydown", onKey);
		return () => document.removeEventListener("keydown", onKey);
	}, []);

	const service = useMemo<ModalService>(
		() => ({ open, close, patch }),
		[open, close, patch],
	);

	return (
		<ModalContext.Provider value={service}>
			{children}
			{pool.map((inst, i) => {
				const Comp = defs[inst.key];
				if (!Comp) return null; // unknown key — skip
				// Lifecycle props win over the args: instances render open, and
				// onCloseRequest closes = unmounts the instance. isTopmost is
				// positional (last in the pool = nothing above it). The key
				// context lets instance components opt into clipboard routing
				// (paste.ts) without knowing their own key.
				return (
					<ModalKeyContext.Provider key={inst.key} value={inst.key}>
						<Comp
							{...inst.args}
							open
							onCloseRequest={() => service.close(inst.key)}
							isTopmost={i === pool.length - 1}
						/>
					</ModalKeyContext.Provider>
				);
			})}
		</ModalContext.Provider>
	);
}
