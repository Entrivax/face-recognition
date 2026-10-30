// Person avatar: the DB thumbnail when present, falling back to the name's
// initials when the image is missing or fails to load (e.g. deleted sidecar).
// Thumbnails are a public route, so a plain <img> URL works everywhere.

import { initials } from "../util";
import { useComputed, useSignal, useSignalEffect } from "@preact/signals";
import { Show, useLiveSignal } from "@preact/signals/utils";

export function Avatar({ src, name }: { src: string; name: string }) {
	const failed = useSignal(false)
	const srcSignal = useLiveSignal(src)

	// A re-selected avatar gets a new URL — retry the image.
	useSignalEffect(() => {
		void srcSignal.value
		failed.value = false
	});

	const imageURL = useComputed(() =>
		failed.value ? undefined : srcSignal.value
	);

	return (
		<Show
			when={imageURL}
			fallback={<span class="person-avatar">{initials(name)}</span>}
		>
			{(url) => (
				<img
					class="person-avatar person-avatar-img"
					src={url}
					alt=""
					onError={() => { failed.value = true; }}
					loading="lazy"
				/>
			)}
		</Show>
	);
}
