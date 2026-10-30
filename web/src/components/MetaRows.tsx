// Row editor for the metadata list fields (aliases, URLs) — shared by the
// person-details edit form and the enroll modal's details section.
//
// One input per entry plus a permanent trailing empty row — no "add" button.
// Filling the ready row spawns the next empty one, and removing rows keeps
// the ready slot at the end. Blank rows (including the ready slot) are
// dropped by the caller before saving.
//
// `entries` is the owning component's signal (ReadonlySignal<string[]>);
// edits read .value and report the next array through onChange — the rows
// stay a plain .map (For tracks items by identity, and two empty rows would
// be indistinguishable keys).

import type { ReadonlySignal } from "@preact/signals";

export function MetaRows(props: {
	/** list state, always ending in the trailing empty row */
	entries: ReadonlySignal<string[]>;
	onChange: (next: string[]) => void;
	/** aria-label for every row input ("Alias" / "URL") */
	label: string;
	placeholder: string;
	maxLength: number;
	disabled?: boolean;
}) {
	const entries = props.entries.value;

	function rowInput(i: number, v: string) {
		const cur = props.entries.value;
		const next = [...cur];
		next[i] = v;
		if (i === cur.length - 1 && v.trim() !== "") next.push("");
		props.onChange(next);
	}
	function rowRemove(i: number) {
		const next = props.entries.value.filter((_, j) => j !== i);
		if (next.length === 0 || next[next.length - 1].trim() !== "") next.push("");
		props.onChange(next);
	}

	return (
		<div class="meta-rows">
			{entries.map((val, i) => (
				<div class="meta-row" key={i}>
					<input
						type="text"
						value={val}
						placeholder={props.placeholder}
						maxLength={props.maxLength}
						aria-label={props.label}
						autocomplete="off"
						spellcheck={false}
						disabled={props.disabled}
						onInput={(e) => rowInput(i, e.currentTarget.value)}
					/>
					{!(i === entries.length - 1 && val.trim() === "") && (
						<button
							type="button"
							class="meta-row-del"
							aria-label="Remove entry"
							disabled={props.disabled}
							onClick={() => rowRemove(i)}
						>
							×
						</button>
					)}
				</div>
			))}
		</div>
	);
}
