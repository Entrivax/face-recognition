// Toast notifications: App (and everything below it) calls useToast().show.
// One #toast element is rendered by the provider, styled by .toast in
// style.css; the message auto-hides after 3.6 s.

import { createContext } from "preact";
import { useContext, useRef } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { batch, useSignal } from "@preact/signals"

export type ToastKind = "" | "ok" | "err";

interface ToastApi {
	show: (msg: string, kind?: ToastKind) => void;
}

const ToastContext = createContext<ToastApi>({ show: () => {} });

export function useToast(): ToastApi {
	return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: ComponentChildren }) {
	const cls = useSignal("toast")
	const msg = useSignal("")
	const timer = useRef<number | undefined>(undefined);

	const show = (m: string, kind: ToastKind = "") => {
		batch(() => {
			msg.value = m
			cls.value = "toast show" + (kind ? " " + kind : "")
		})
		clearTimeout(timer.current);
		timer.current = window.setTimeout(() => cls.value = "toast", 3600);
	};

	return (
		<ToastContext.Provider value={{ show }}>
			{children}
			<div id="toast" class={cls} role="status" aria-live="polite">{msg}</div>
		</ToastContext.Provider>
	);
}
