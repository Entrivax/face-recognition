// Right panel: the enrolled-people list (with search filter), the match
// threshold, and the role-gated actions. Every viewer (including a share
// visitor) may browse the list and open a person's details; what else is
// offered follows the viewer's role in the selected repository — writers and
// admins get enroll/rescan/remove, admins get the threshold slider (readers
// see the current value as read-only text), compare is open to any
// signed-in user. The filter matches names and aliases; when only an alias
// matched, the row shows that alias ("aka") so the hit explains itself.
// Data comes from the app store as signals (perms included); the dialogs
// open through the modal service.

import type { TargetedEvent } from "preact";
import { useComputed, useSignal, useSignalEffect } from "@preact/signals";
import { For, Show } from "@preact/signals/utils";
import { useApp } from "../appStore";
import { useModals } from "../modalRegistry";
import { Avatar } from "./Avatar";
import { foldText } from "../util";

export function PeoplePanel() {
	const app = useApp();
	const modals = useModals();

	// Component-local state as signals: the search text, the slider's
	// display value and the rescan flag. Everything derived is a computed.
	const query = useSignal("");
	const slider = useSignal("0.45");
	const rescanning = useSignal(false);

	// Mirror the server's persisted threshold into the slider (signal reads
	// have stable identity — the effect is driven by the value change).
	useSignalEffect(() => {
		const threshold = app.threshold.value;
		if (threshold !== null) slider.value = Number(threshold).toFixed(2);
	});

	const countText = useComputed(() => {
		const people = app.people.value;
		const count = people.length;
		return app.peopleErr.value
			? "Could not load people."
			: count
				? `${count} ${count === 1 ? "person" : "people"} in this repository`
				: "No one enrolled yet.";
	});

	// Case- and diacritics-insensitive substring filter across names and
	// aliases (foldText: "Émile" matches "emil"); when only an alias matched,
	// the row displays it (aka) so the hit is explainable. An all-filtered-out
	// list shows a hint row.
	const filtered = useComputed(() => {
		const q = foldText(query.value.trim());
		const people = app.people.value;
		if (!q) return people.map((p) => ({ p, aka: null as string | null }));
		return people.flatMap((p) => {
			const m = rowMatch(p, q);
			return m ? [{ p, aka: m.aka }] : [];
		});
	});
	const noMatch = useComputed(() =>
		query.value.trim() !== "" && app.people.value.length > 0 && filtered.value.length === 0,
	);

	const doRescan = async () => {
		rescanning.value = true;
		try {
			await app.rescan();
		} finally {
			rescanning.value = false;
		}
	};

	const searchInput = (e: TargetedEvent<HTMLInputElement>) => { query.value = e.currentTarget.value; };
	const slideInput = (e: TargetedEvent<HTMLInputElement>) => { slider.value = e.currentTarget.value; };
	const commitThreshold = () => void app.commitThreshold(Number(slider.value));

	return (
		<aside class="panel people" aria-labelledby="peopleTitle">
			<div class="panel-head">
				<h2 id="peopleTitle">Enrolled people</h2>
				<p class="lede" id="peopleCount">{countText}</p>
			</div>

			{/* threshold row: admin edits (slider), everyone else sees the
			    current value — both variants exist only once the server told
			    us the threshold */}
			<Show when={app.threshold} fallback={null}>
				{(threshold) => (
					<Show
						when={() => app.perms.value.admin}
						fallback={
							<div
								class="threshold-row"
								title={app.perms.value.user ? "Only administrators can change the match threshold" : undefined}
							>
								<span class="threshold-label">Threshold</span>
								<span class="threshold-static">{Number(threshold).toFixed(2)}</span>
							</div>
						}
					>
						{() => (
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
									onChange={commitThreshold}
								/>
								<span class="threshold-value" id="thresholdValue">{Number(slider.value).toFixed(2)}</span>
							</div>
						)}
					</Show>
				)}
			</Show>

			<Show when={() => app.perms.value.writer || app.perms.value.user} fallback={null}>
				{() => (
					<div class="people-actions">
						<Show when={() => app.perms.value.writer} fallback={null}>
							{() => (
								<button id="enrollBtn" class="btn btn-accent" aria-haspopup="dialog" onClick={() => modals.open("enroll")}>
									Enroll new person
								</button>
							)}
						</Show>
						<Show when={() => app.perms.value.user} fallback={null}>
							{() => (
								<button id="compareBtn" class="btn" aria-haspopup="dialog" onClick={() => modals.open("compare")}>
									Compare two photos
								</button>
							)}
						</Show>
					</div>
				)}
			</Show>

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
				<For each={filtered} getKey={(row) => row.p.id}>
					{({ p, aka }) => (
						<PersonRow
							person={p}
							aka={aka}
							onOpen={(name) => modals.open("person", { name })}
							onRemove={(name) => void app.removePerson(name)}
						/>
					)}
				</For>
				<Show when={noMatch} fallback={null}>
					{() => <li class="people-nomatch">No people match “{query.value.trim()}”.</li>}
				</Show>
			</ul>

			<Show when={() => app.perms.value.writer} fallback={null}>
				{() => (
					<div class="people-footer">
						<button
							id="rescanBtn"
							class="btn btn-ghost"
							title="Re-scan this repository's dataset folder on the server"
							disabled={rescanning}
							onClick={doRescan}
						>
							{rescanning.value ? "Rescanning…" : "Rescan people folder"}
						</button>
					</div>
				)}
			</Show>
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
function rowMatch(p: PersonSummaryLike, q: string): { aka: string | null } | null {
	if (foldText(p.name).includes(q)) return { aka: null };
	const a = (p.aliases ?? []).find((al) => foldText(al).includes(q));
	return a ? { aka: a } : null;
}

type PersonSummaryLike = import("../types").PersonSummary;

function PersonRow(props: {
	person: PersonSummaryLike;
	/** matching alias, when the query hit an alias but not the name */
	aka: string | null;
	onOpen: (name: string) => void;
	onRemove: (name: string) => void;
}) {
	const app = useApp();
	const canRemove = app.perms.value.writer;
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
			{canRemove && (
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
