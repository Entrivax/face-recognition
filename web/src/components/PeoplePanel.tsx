// Right panel: the enrolled-people list (with search filter), the match
// threshold, and the role-gated actions. Every viewer (including a share
// visitor) may browse the list and open a person's details; what else is
// offered follows the viewer's role in the selected repository — writers and
// admins get enroll/rescan/remove, admins get the threshold slider (readers
// see the current value as read-only text), compare is open to any
// signed-in user. The filter matches names and aliases; when only an alias
// matched, the row shows that alias ("aka") so the hit explains itself.

import { useEffect, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import type { PersonSummary } from "../types";
import { Avatar } from "./Avatar";
import { foldText } from "../util";

/** What the viewer (signed-in user or share visitor) may do in this repo. */
export interface Perms {
	/** a signed-in user (false for share visitors) */
	user: boolean;
	/** may enroll/modify people in the selected repo (writer or admin) */
	writer: boolean;
	/** server administrator */
	admin: boolean;
}

interface PeoplePanelProps {
	perms: Perms;
	people: PersonSummary[];
	peopleErr: boolean;
	/** server-persisted threshold; null = unknown (share mode) → row hidden */
	threshold: number | null;
	onThresholdCommit: (v: number) => void;
	onRescan: () => Promise<void>;
	onOpenPerson: (name: string) => void;
	onRemove: (name: string) => void;
	onEnrollClick: () => void;
	onCompareClick: () => void;
}

export function PeoplePanel(props: PeoplePanelProps) {
	const {
		perms,
		people, peopleErr, threshold,
		onThresholdCommit, onRescan,
		onOpenPerson, onRemove,
	} = props;

	const [query, setQuery] = useState("");
	const [slider, setSlider] = useState(() => threshold === null ? "0.45" : Number(threshold).toFixed(2));
	const [rescanning, setRescanning] = useState(false);

	// Mirror the server's persisted threshold into the slider.
	useEffect(() => {
		if (threshold !== null) setSlider(Number(threshold).toFixed(2));
	}, [threshold]);

	const count = people.length;
	const countText = peopleErr
		? "Could not load people."
		: count
			? `${count} ${count === 1 ? "person" : "people"} in this repository`
			: "No one enrolled yet.";

	// Case- and diacritics-insensitive substring filter across names and
	// aliases (foldText: "Émile" matches "emil"); when only an alias matched,
	// the row displays it (aka) so the hit is explainable. An all-filtered-out
	// list shows a hint row.
	const q = foldText(query.trim());
	const filtered = q
		? people.flatMap((p) => {
				const m = rowMatch(p, q);
				return m ? [{ p, aka: m.aka }] : [];
			})
		: people.map((p) => ({ p, aka: null as string | null }));
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

			{perms.admin && threshold !== null && (
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
						onChange={() => onThresholdCommit(Number(slider))}
					/>
					<span class="threshold-value" id="thresholdValue">{Number(slider).toFixed(2)}</span>
				</div>
			)}
			{!perms.admin && threshold !== null && (
				<div
					class="threshold-row"
					title={perms.user ? "Only administrators can change the match threshold" : undefined}
				>
					<span class="threshold-label">Threshold</span>
					<span class="threshold-static">{Number(threshold).toFixed(2)}</span>
				</div>
			)}

			{(perms.writer || perms.user) && (
				<div class="people-actions">
					{perms.writer && (
						<button id="enrollBtn" class="btn btn-accent" aria-haspopup="dialog" onClick={props.onEnrollClick}>
							Enroll new person
						</button>
					)}
					{perms.user && (
						<button id="compareBtn" class="btn" aria-haspopup="dialog" onClick={props.onCompareClick}>
							Compare two photos
						</button>
					)}
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
				{filtered.map(({ p, aka }) => (
					<PersonRow
						key={p.id}
						person={p}
						aka={aka}
						canRemove={perms.writer}
						onOpen={onOpenPerson}
						onRemove={onRemove}
					/>
				))}
				{noMatch && <li class="people-nomatch">No people match “{query.trim()}”.</li>}
			</ul>

			{perms.writer && (
				<div class="people-footer">
					<button
						id="rescanBtn"
						class="btn btn-ghost"
						title="Re-scan this repository's dataset folder on the server"
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

/**
 * rowMatch reports how a row matches the (folded) query: null when it
 * doesn't match at all, {aka: null} when the name matched (nothing extra to
 * show), {aka: alias} when only an alias matched — the row then displays it.
 * (The name-match and no-match cases must be distinguishable: collapsing
 * them into one null filtered out every name-matched row.) Both sides are
 * folded (foldText), so matching is case- and diacritics-insensitive.
 */
function rowMatch(p: PersonSummary, q: string): { aka: string | null } | null {
	if (foldText(p.name).includes(q)) return { aka: null };
	const a = (p.aliases ?? []).find((al) => foldText(al).includes(q));
	return a ? { aka: a } : null;
}

function PersonRow(props: {
	person: PersonSummary;
	/** matching alias, when the query hit an alias but not the name */
	aka: string | null;
	/** may remove people from this repo (writer or admin) */
	canRemove: boolean;
	onOpen: (name: string) => void;
	onRemove: (name: string) => void;
}) {
	const p = props.person;
	// Every row opens the person details modal; the remove button stays
	// writer/admin-only.
	return (
		<li class="person-row">
			<button
				type="button"
				class="person-avatar-btn"
				title="View details"
				aria-label={`View details for ${p.name}`}
				onClick={() => props.onOpen(p.name)}
			>
				<Avatar src={p.thumb} name={p.name} />
			</button>
			<button
				type="button"
				class="person-open"
				title="View details"
				aria-label={`View details for ${p.name}`}
				onClick={() => props.onOpen(p.name)}
			>
				<span class="person-name">{p.name}</span>
				<span class="person-count">{p.photos} photo(s)</span>
				{props.aka && <span class="person-aka">aka {props.aka}</span>}
			</button>
			{props.canRemove && (
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
