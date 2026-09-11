// Right panel: the enrolled-people list (with search filter), the match
// threshold slider, the enroll entry point, and the people-folder rescan.
// The gallery itself is public (the list endpoint is); the threshold,
// rescan, enroll, remove and open-photos controls are admin-only and only
// render while logged in.

import { useEffect, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import type { PersonSummary } from "../types";
import { Avatar } from "./Avatar";

interface PeoplePanelProps {
	/** logged in? gates the admin-only controls (list itself is public) */
	authed: boolean;
	people: PersonSummary[];
	peopleErr: boolean;
	/** server-persisted threshold (mirrored by the slider) */
	threshold: number;
	onThresholdCommit: (v: number) => void;
	onRescan: () => Promise<void>;
	onOpenPhotos: (name: string) => void;
	onRemove: (name: string) => void;
	onEnrollClick: () => void;
}

export function PeoplePanel(props: PeoplePanelProps) {
	const {
		authed,
		people, peopleErr, threshold, onRescan,
		onOpenPhotos, onRemove,
	} = props;

	const [query, setQuery] = useState("");
	const [slider, setSlider] = useState(() => Number(threshold).toFixed(2));
	const [rescanning, setRescanning] = useState(false);

	// Mirror the server's persisted threshold into the slider.
	useEffect(() => setSlider(Number(threshold).toFixed(2)), [threshold]);

	const count = people.length;
	const countText = peopleErr
		? "Could not load people."
		: count
			? `${count} ${count === 1 ? "person" : "people"} in the database`
			: "No one enrolled yet.";

	// Case-insensitive substring filter; an all-filtered-out list shows a hint row.
	const q = query.trim().toLowerCase();
	const filtered = q ? people.filter((p) => p.name.toLowerCase().includes(q)) : people;
	const noMatch = q !== "" && count > 0 && filtered.length === 0;

	const doRescan = async () => {
		setRescanning(true);
		try {
			await onRescan();
		} finally {
			setRescanning(false);
		}
	};

	const searchInput = (e: TargetedEvent<HTMLInputElement>) => setQuery(e.currentTarget.value);
	const slideInput = (e: TargetedEvent<HTMLInputElement>) => setSlider(e.currentTarget.value);

	return (
		<aside class="panel people" aria-labelledby="peopleTitle">
			<div class="panel-head">
				<h2 id="peopleTitle">Enrolled people</h2>
				<p class="lede" id="peopleCount">{countText}</p>
			</div>

			{authed && (
				<div class="threshold-row">
					<label
						class="threshold-label"
						for="thresholdSlider"
						title="Minimum cosine similarity for a positive match — higher means fewer false positives"
					>
						Threshold
					</label>
					<input
						id="thresholdSlider"
						type="range"
						min="0.30"
						max="0.70"
						step="0.01"
						value={slider}
						aria-label="Match threshold"
						onInput={slideInput}
						onChange={() => props.onThresholdCommit(Number(slider))}
					/>
					<span class="threshold-value" id="thresholdValue">{Number(slider).toFixed(2)}</span>
				</div>
			)}

			{authed && (
				<div class="people-actions">
					<button id="enrollBtn" class="btn btn-accent" aria-haspopup="dialog" onClick={props.onEnrollClick}>
						Enroll new person
					</button>
				</div>
			)}

			<div class="people-search">
				<input
					id="peopleSearch"
					type="search"
					placeholder="Filter people…"
					autocomplete="off"
					spellcheck={false}
					aria-label="Filter people by name"
					value={query}
					onInput={searchInput}
				/>
			</div>

			<ul id="peopleList" class="people-list">
				{filtered.map((p) => (
					<PersonRow key={p.id} person={p} authed={authed} onOpen={onOpenPhotos} onRemove={onRemove} />
				))}
				{noMatch && <li class="people-nomatch">No people match “{query.trim()}”.</li>}
			</ul>

			{authed && (
				<div class="people-footer">
					<button
						id="rescanBtn"
						class="btn btn-ghost"
						title="Re-scan the people/ folder on the server"
						disabled={rescanning}
						onClick={doRescan}
					>
						{rescanning ? "Rescanning…" : "Rescan people folder"}
					</button>
				</div>
			)}
		</aside>
	);
}

function PersonRow(props: {
	person: PersonSummary;
	authed: boolean;
	onOpen: (name: string) => void;
	onRemove: (name: string) => void;
}) {
	const p = props.person;
	const authed = props.authed;
	return (
		<li class="person-row">
			{authed ? (
				<button
					type="button"
					class="person-avatar-btn"
					title="Manage photos"
					aria-label={`Manage photos for ${p.name}`}
					onClick={() => props.onOpen(p.name)}
				>
					<Avatar src={p.thumb} name={p.name} />
				</button>
			) : (
				<span class="person-avatar-holder"><Avatar src={p.thumb} name={p.name} /></span>
			)}
			{authed ? (
				<button
					type="button"
					class="person-open"
					title="Manage photos"
					aria-label={`Manage photos for ${p.name}`}
					onClick={() => props.onOpen(p.name)}
				>
					<span class="person-name">{p.name}</span>
					<span class="person-count">{p.photos} photo(s)</span>
				</button>
			) : (
				<span class="person-open static">
					<span class="person-name">{p.name}</span>
					<span class="person-count">{p.photos} photo(s)</span>
				</span>
			)}
			{authed && (
				<button
					class="person-del"
					title={`Remove ${p.name}`}
					aria-label={`Remove ${p.name}`}
					onClick={() => props.onRemove(p.name)}
				>
					×
				</button>
			)}
		</li>
	);
}
