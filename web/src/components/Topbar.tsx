// Topbar: brand mark + live server status pill.

import type { Health } from "../types";

export function Topbar({ health, err }: { health: Health | null; err: boolean }) {
	const cls = err ? "status err" : health ? "status ok" : "status";
	const text = err
		? "server unreachable"
		: health
			? `${health.people} people · threshold ${health.threshold.toFixed(2)}`
			: "connecting…";
	return (
		<header class="topbar">
			<div class="brand">
				<span class="brand-mark" aria-hidden="true">
					<img src="/logo.svg" />
				</span>
				<span class="brand-word">recogn</span>
				<span class="brand-sub">face review console</span>
			</div>
			<div class={cls} id="status">
				<span class="status-dot" id="statusDot" />
				<span id="statusText">{text}</span>
			</div>
		</header>
	);
}
