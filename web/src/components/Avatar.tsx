// Person avatar: the DB thumbnail when present, falling back to the name's
// initials when the image is missing or fails to load (e.g. deleted sidecar).
//
// In share mode the thumbnail cannot be a plain <img> URL — the share token
// rides a request header that an <img> element cannot carry — so the bytes
// are fetched through the API wrapper (header attached, no cookies) and
// handed to the DOM as an object URL instead.

import { useEffect, useState } from "preact/hooks";
import { getShareContext } from "../api/core";
import { fetchThumbObjectURL } from "../api/people";
import { initials } from "../util";

export function Avatar({ src, name }: { src: string; name: string }) {
	const [failed, setFailed] = useState(false);
	const [fetchedURL, setFetchedURL] = useState<string | null>(null);
	// Read once per render: the share context is set before ShareView mounts,
	// and nothing below re-reads it afterwards.
	const share = getShareContext() !== null;

	// A re-selected avatar gets a new URL — retry the image.
	useEffect(() => {
		setFailed(false);
		setFetchedURL(null);
	}, [src]);

	// Share mode: fetch the thumbnail through the wrapper; the object URL is
	// revoked on replacement or unmount so no thumbnail stays pinned.
	useEffect(() => {
		if (!share || !src) return;
		let alive = true;
		let url: string | null = null;
		fetchThumbObjectURL(src)
			.then((u) => {
				if (!alive) {
					URL.revokeObjectURL(u);
					return;
				}
				url = u;
				setFetchedURL(u);
			})
			.catch(() => {
				if (alive) setFailed(true);
			});
		return () => {
			alive = false;
			if (url) URL.revokeObjectURL(url);
		};
	}, [src, share]);

	if (share) {
		if (failed || !fetchedURL) {
			return <span class="person-avatar">{initials(name)}</span>;
		}
		return <img class="person-avatar person-avatar-img" src={fetchedURL} alt="" onError={() => setFailed(true)} />;
	}
	if (!src || failed) {
		return <span class="person-avatar">{initials(name)}</span>;
	}
	return <img class="person-avatar person-avatar-img" src={src} alt="" onError={() => setFailed(true)} />;
}
