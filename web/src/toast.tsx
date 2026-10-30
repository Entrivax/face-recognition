// Toast notifications: App (and everything below it) calls useToast().show.
// One #toast element is rendered by the provider, styled by .toast in
// style.css; the message auto-hides after 3.6 s.

import { createContext } from "preact";
import { useContext } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { batch, createModel, signal, useModel, type ReadonlySignal } from "@preact/signals"

export type ToastKind = "" | "ok" | "err";

interface ToastApi {
	show: (msg: string, kind?: ToastKind) => void;
}

const ToastContext = createContext<ToastApi>({ show: () => {} });

export function useToast(): ToastApi {
	return useContext(ToastContext);
}

interface Toast {
	classes: ReadonlySignal<string>
	msg: ReadonlySignal<string>
	show(message: string, kind?: ToastKind): void
}

const ToastModel = createModel<Toast>(() => {
	const classes = signal("toast")
	const msg = signal("")
	const timer = signal<number | undefined>(undefined)
	return {
		classes,
		msg,
		show(message: string, kind: ToastKind = "") {
			batch(() => {
				msg.value = message
				classes.value = "toast show" + (kind ? " " + kind : "")
			})
			clearTimeout(timer.peek())
			timer.value = window.setTimeout(() => classes.value = "toast", 3600)
		}
	}
})

export function ToastProvider({ children }: { children: ComponentChildren }) {
	const toast = useModel(ToastModel)

	return (
		<ToastContext.Provider value={{ show: toast.show }}>
			{children}
			<div id="toast" class={toast.classes} role="status" aria-live="polite">{toast.msg}</div>
		</ToastContext.Provider>
	);
}
