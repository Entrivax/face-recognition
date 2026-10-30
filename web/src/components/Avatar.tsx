// Person avatar: the DB thumbnail when present, falling back to the name's
// initials when the image is missing or fails to load (e.g. deleted sidecar).
//
// In share mode the thumbnail cannot be a plain <img> URL — the share token
// rides a request header that an <img> element cannot carry — so the bytes
// are fetched through the API wrapper (header attached, no cookies) and
// handed to the DOM as an object URL instead.

import { getShareContext } from "../api/core";
import { fetchThumbObjectURL } from "../api/people";
import { initials } from "../util";
import { batch, useComputed, useSignal, useSignalEffect } from "@preact/signals";
import { Show, useLiveSignal } from "@preact/signals/utils";

export function Avatar({ src, name }: { src: string; name: string }) {
	const failed = useSignal(false)
	const fetchedURL = useSignal<string | undefined>(undefined)
	const srcSignal = useLiveSignal(src)
	// Read once per render: the share context is set before ShareView mounts,
	// and nothing below re-reads it afterwards.
	const share = getShareContext() !== null

	// A re-selected avatar gets a new URL — retry the image.
	useSignalEffect(() => {
		void srcSignal.value
		batch(() => {
			failed.value = false
			fetchedURL.value = undefined
		})
	});

	// Share mode: fetch the thumbnail through the wrapper; the object URL is
	// revoked on replacement or unmount so no thumbnail stays pinned.
	useSignalEffect(() => {
		if (!share || !srcSignal.value) return;
		let alive = true;
		let url: string | undefined = undefined;
		fetchThumbObjectURL(srcSignal.value)
			.then((u) => {
				if (!alive) {
					URL.revokeObjectURL(u);
					return;
				}
				url = u;
				fetchedURL.value = u
			})
			.catch(() => {
				if (alive) failed.value = true
			});
		return () => {
			alive = false;
			if (url) URL.revokeObjectURL(url);
		};
	});

	const imageURL = useComputed(() =>
		failed.value ? undefined : share ? fetchedURL.value : srcSignal.value
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
