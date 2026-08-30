/* recogn — small shared helpers. */

import { el } from "./dom.js";

let toastTimer = null;

export function showToast(msg, kind = "") {
	el.toast.textContent = msg;
	el.toast.className = "toast show" + (kind ? " " + kind : "");
	clearTimeout(toastTimer);
	toastTimer = setTimeout(() => (el.toast.className = "toast"), 3600);
}

export function escapeHtml(s) {
	return String(s).replace(/[&<>"']/g, (c) => ({
		"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
	}[c]));
}

export function fmtSize(bytes) {
	return bytes >= 1024 * 1024
		? `${(bytes / (1024 * 1024)).toFixed(1)} MB`
		: `${Math.max(1, Math.round(bytes / 1024))} KB`;
}

export function initials(name) {
	return name.trim().split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
}
