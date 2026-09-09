// Person avatar: the DB thumbnail when present, falling back to the name's
// initials when the image is missing or fails to load (e.g. deleted sidecar).

import { useEffect, useState } from "preact/hooks";
import { initials } from "../util";

export function Avatar({ src, name }: { src: string; name: string }) {
	const [failed, setFailed] = useState(false);

	// A re-selected avatar gets a new URL — retry the image.
	useEffect(() => setFailed(false), [src]);

	if (!src || failed) {
		return <span class="person-avatar">{initials(name)}</span>;
	}
	return (
		<img class="person-avatar person-avatar-img" src={src} alt="" onError={() => setFailed(true)} />
	);
}
